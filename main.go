package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"sync/atomic"
	"time"

	"github.com/go-ble/ble"
	"github.com/go-ble/ble/linux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	listen   = flag.String("listen", ":2112", "http addr/port to listen on")
	addr     = flag.String("addr", "", "bluetooth MAC address of the bowl")
	sd       = flag.Duration("sd", 1*time.Minute, "scanning duration")
	interval = flag.Duration("interval", 10*time.Minute, "polling interval")

	mBat1     = promauto.NewGauge(prometheus.GaugeOpts{Name: "bowl_battery1_percent"})
	mBat2     = promauto.NewGauge(prometheus.GaugeOpts{Name: "bowl_battery2_percent"})
	mWeight   = promauto.NewGauge(prometheus.GaugeOpts{Name: "bowl_weight_grams"})
	mLast     = promauto.NewGauge(prometheus.GaugeOpts{Name: "bowl_last_update_seconds"})
	mErrors   = promauto.NewGauge(prometheus.GaugeOpts{Name: "bowl_update_errors_total"})
	mAttempts = promauto.NewGauge(prometheus.GaugeOpts{Name: "bowl_update_attempts_total"})

	bowlService        = ble.MustParse("0179bbd0535148b5bf6d2167639bc867")
	bowlCharacteristic = ble.MustParse("0179bbd1535148b5bf6d2167639bc867")
	btTimeout          = 10 * time.Minute
	watchdogTimeout    = 25 * time.Minute

	lastAttemptTime atomic.Value
)

type data struct {
	battery1 int // battery level as reported by default bt service.
	battery2 int // battery level as reported by the bowl's custom service.
	weight   int // weight in grams.
}

func main() {
	flag.Parse()

	if *addr == "" {
		log.Fatal("Please provide --addr")
	}

	http.Handle("/metrics", promhttp.Handler())
	go func() {
		log.Printf("Listening on %s", *listen)
		log.Fatal(http.ListenAndServe(*listen, nil))
	}()

	go func() {
		for {
			if la := lastAttemptTime.Load(); la != nil {
				if time.Since(la.(time.Time)) > watchdogTimeout {
					log.Printf("Last attempt was too long ago (%s); exiting", la.(time.Time))
					os.Exit(1)
				}
			}
			time.Sleep(10 * time.Second)
		}
	}()

	for {
		start := time.Now()
		d, err := attempt()
		mAttempts.Inc()
		lastAttemptTime.Store(time.Now())
		if err != nil {
			log.Printf("ERROR: %s", err)
			mErrors.Inc()
			continue
		}
		mBat1.Set(float64(d.battery1))
		mBat2.Set(float64(d.battery2))
		mWeight.Set(float64(d.weight))
		mLast.SetToCurrentTime()
		time.Sleep(time.Until(start.Add(*interval)))
	}
}

func attempt() (*data, error) {
	d, err := linux.NewDevice(ble.OptDialerTimeout(btTimeout), ble.OptListenerTimeout(btTimeout))
	if err != nil {
		log.Fatalf("can't init device: %s", err)
	}
	ble.SetDefaultDevice(d)
	defer d.Stop()

	log.Printf("Scanning for %s...", *sd)
	ctx := ble.WithSigHandler(context.WithTimeout(context.Background(), *sd))
	cln, err := ble.Connect(ctx, func(a ble.Advertisement) bool { return ble.NewAddr(*addr) == a.Addr() })
	if err != nil {
		return nil, fmt.Errorf("can't connect: %w", err)
	}

	log.Printf("Connected to %s", cln.Addr())
	done := make(chan struct{})
	go func() {
		<-cln.Disconnected()
		log.Printf("Disconnected from %s", cln.Addr())
		close(done)
	}()

	svcs, err := cln.DiscoverServices([]ble.UUID{ble.BatteryUUID, bowlService})
	if err != nil {
		return nil, fmt.Errorf("can't discover services: %s", err)
	}

	result := &data{}
	for _, s := range svcs {
		chars, err := cln.DiscoverCharacteristics(nil, s)
		if err != nil {
			return nil, fmt.Errorf("could not read characteristics: %s", err)
		}
		if s.UUID.Equal(ble.BatteryUUID) && len(chars) > 0 {
			char := s.Characteristics[0]
			if !char.UUID.Equal(ble.UUID16(0x2A19)) {
				return nil, fmt.Errorf("unexpected battery characteristic: %s", char.UUID)
			}
			v, err := cln.ReadCharacteristic(char)
			if err != nil {
				return nil, fmt.Errorf("could not read battery characteristic: %w", err)
			}
			bat := int(v[0])
			log.Printf("battery level: %d", bat)
			result.battery1 = bat
			continue
		}
		if !s.UUID.Equal(bowlService) || len(chars) == 0 {
			log.Printf("skipping service %s (%d characteristics)", s.UUID.String(), len(chars))
			continue
		}
		char := chars[0]
		if !char.UUID.Equal(bowlCharacteristic) {
			return nil, fmt.Errorf("unexpected bowl characteristic: %s", char.UUID)
		}

		if _, err := cln.DiscoverDescriptors(nil, char); err != nil {
			return nil, fmt.Errorf("cannot discover descriptors: %s", err)
		}

		var lastSent atomic.Value
		h := func(req []byte) {
			log.Printf("Received: %q [% X]", string(req), req)
			last := lastSent.Load().(string)
			if len(req) == 6 && req[3] == 0x88 && last == "88" {
				bat := int(req[0])
				log.Printf("battery level: %d", bat)
				result.battery2 = bat
			}
			if len(req) == 3 && last == "8000" {
				weight := int16(binary.BigEndian.Uint16(req[1:]))
				log.Printf("weight: %d", weight)
				result.weight = int(weight)
			}
		}
		if err := cln.Subscribe(char, false, h); err != nil {
			return nil, fmt.Errorf("subscribe failed: %s", err)
		}
		time.Sleep(time.Second)

		msgs := []string{
			"82",   // some sort of init/hello message.
			"88",   // request battery level.
			"8000", // request weight.
		}
		for _, msg := range msgs {
			log.Printf("Writing %s", msg)
			b, err := hex.DecodeString("0D0A" + msg + "0D0A")
			if err != nil {
				return nil, err
			}
			if err := cln.WriteCharacteristic(char, b, true); err != nil {
				return nil, err
			}
			lastSent.Store(msg)
			time.Sleep(3 * time.Second)
		}

		if err := cln.Unsubscribe(char, false); err != nil {
			return nil, fmt.Errorf("unsubscribe failed: %s", err)
		}
	}

	if err := cln.CancelConnection(); err != nil {
		log.Printf("CancelConnection returned error: %s", err)
	}

	<-done
	return result, nil
}
