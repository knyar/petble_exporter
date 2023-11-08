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
	"sync"
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
	sd       = flag.Duration("sd", 3*time.Minute, "duration of each scanning attempt")
	interval = flag.Duration("interval", 10*time.Minute, "metric collection interval")

	mBat1     = promauto.NewGauge(prometheus.GaugeOpts{Name: "bowl_battery1_percent"})
	mBat2     = promauto.NewGauge(prometheus.GaugeOpts{Name: "bowl_battery2_percent"})
	mWeight   = promauto.NewGauge(prometheus.GaugeOpts{Name: "bowl_weight_grams"})
	mLast     = promauto.NewGauge(prometheus.GaugeOpts{Name: "bowl_last_update_seconds"})
	mErrors   = promauto.NewGauge(prometheus.GaugeOpts{Name: "bowl_update_errors_total"})
	mAttempts = promauto.NewGauge(prometheus.GaugeOpts{Name: "bowl_update_attempts_total"})

	bowlService        = ble.MustParse("0179bbd0535148b5bf6d2167639bc867")
	bowlCharacteristic = ble.MustParse("0179bbd1535148b5bf6d2167639bc867")

	lastAttemptTime atomic.Value

	mu sync.Mutex
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
	http.HandleFunc("/zero", zeroHandler)
	http.HandleFunc("/refresh", refreshHandler)
	go func() {
		log.Printf("Listening on %s", *listen)
		log.Fatal(http.ListenAndServe(*listen, nil))
	}()

	go func() {
		var watchdogTimeout = *interval * 3
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
		d, err := refresh(log.Printf)
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
		if time.Now().Year() > 2022 {
			// Raspberry pi might not have synchronized time yet after a reboot,
			// so only set the last-update metric if current time is recent enough.
			mLast.SetToCurrentTime()
		}
		time.Sleep(time.Until(start.Add(*interval)))
	}
}

// zeroHandler sends a command to set the scales to zero.
func zeroHandler(w http.ResponseWriter, r *http.Request) {
	logf := func(format string, args ...any) {
		log.Printf(format, args...)
		w.Write([]byte(fmt.Sprintf(format, args...) + "\n"))
	}
	session(logf, []string{"79"}, nil)
}

// refreshHandler triggers a refresh manually.
func refreshHandler(w http.ResponseWriter, r *http.Request) {
	logf := func(format string, args ...any) {
		log.Printf(format, args...)
		w.Write([]byte(fmt.Sprintf(format, args...) + "\n"))
	}
	refresh(logf)
}

func refresh(logf func(format string, args ...any)) (*data, error) {
	cmds := []string{
		"82",   // some sort of init/hello message.
		"88",   // request battery level.
		"8000", // request weight.
	}
	result := &data{}
	h := func(req []byte, last string) {
		if len(req) == 6 && req[3] == 0x88 && last == "88" {
			bat := int(req[0])
			logf("battery level: %d", bat)
			result.battery2 = bat
		}
		if len(req) == 3 && last == "8000" {
			weight := int16(binary.BigEndian.Uint16(req[1:]))
			logf("weight: %d", weight)
			result.weight = int(weight)
		}
	}

	stats, err := session(logf, cmds, h)
	if err != nil {
		return nil, err
	}
	result.battery1 = stats.battery
	return result, nil
}

type sessionStats struct {
	battery int
}

// session runs a series of commands, calling the callback function on every response.
func session(logf func(format string, args ...any), cmds []string, h func([]byte, string)) (*sessionStats, error) {
	mu.Lock()
	defer mu.Unlock()

	d, err := linux.NewDevice(ble.OptDialerTimeout(*interval), ble.OptListenerTimeout(*interval))
	if err != nil {
		log.Fatalf("can't init device: %s", err)
	}
	ble.SetDefaultDevice(d)
	defer d.Stop()

	logf("Scanning for %s, looking for %s...", *sd, *addr)
	ctx := ble.WithSigHandler(context.WithTimeout(context.Background(), *sd))
	seen := make(map[string]bool)
	cln, err := ble.Connect(ctx, func(a ble.Advertisement) bool {
		this := a.Addr().String()
		good := ble.NewAddr(*addr).String() == this
		if !seen[this] {
			logf("Seeing %s; want=%v", a.Addr(), good)
			seen[this] = true
		}
		return good
	})
	if err != nil {
		return nil, fmt.Errorf("can't connect: %w", err)
	}

	logf("Connected to %s", cln.Addr())
	done := make(chan struct{})
	go func() {
		<-cln.Disconnected()
		logf("Disconnected from %s", cln.Addr())
		close(done)
	}()

	svcs, err := cln.DiscoverServices([]ble.UUID{ble.BatteryUUID, bowlService})
	if err != nil {
		return nil, fmt.Errorf("can't discover services: %s", err)
	}

	stats := &sessionStats{}
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
			logf("battery level: %d", bat)
			stats.battery = bat
			continue
		}
		if !s.UUID.Equal(bowlService) || len(chars) == 0 {
			logf("skipping service %s (%d characteristics)", s.UUID.String(), len(chars))
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
		subh := func(req []byte) {
			logf("Received: %q [% X]", string(req), req)
			if h != nil {
				last := lastSent.Load().(string)
				h(req, last)
			}
		}
		if err := cln.Subscribe(char, false, subh); err != nil {
			return nil, fmt.Errorf("subscribe failed: %s", err)
		}
		time.Sleep(time.Second)

		for idx, cmd := range cmds {
			logf("Writing %s", cmd)
			b, err := hex.DecodeString("0D0A" + cmd + "0D0A")
			if err != nil {
				return nil, err
			}
			if err := cln.WriteCharacteristic(char, b, true); err != nil {
				return nil, err
			}
			lastSent.Store(cmd)
			if idx < (len(cmds) - 1) {
				time.Sleep(2 * time.Second)
			} else {
				// Wait slightly longer after the last command.
				time.Sleep(5 * time.Second)
			}
		}

		if err := cln.Unsubscribe(char, false); err != nil {
			return nil, fmt.Errorf("unsubscribe failed: %s", err)
		}
	}

	if err := cln.CancelConnection(); err != nil {
		logf("CancelConnection returned error: %s", err)
	}

	<-done
	return stats, nil
}
