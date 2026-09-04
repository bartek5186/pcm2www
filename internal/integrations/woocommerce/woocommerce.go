// internal/integrations/woocommerce/woocommerce.go
package woocommerce

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bartek5186/pcm2www/internal/integrations"
	"github.com/rs/zerolog"
)

type WooCache struct {
	PrimeOnStart         bool   `json:"prime_on_start"`
	SweepIntervalMinutes int    `json:"sweep_interval_minutes"`
	Fields               string `json:"fields"`
}

type Config struct {
	BaseURL      string              `json:"base_url"` // https://shop.example.com
	ConsumerKey  string              `json:"consumer_key"`
	ConsumerSec  string              `json:"consumer_secret"`
	PollSec      int                 `json:"poll_sec"` // co ile sekund worker sprawdza kolejkę
	Workers      int                 `json:"workers"`  // liczba równoległych workerów (domyślnie 3)
	Cache        WooCache            `json:"cache"`
	CustomFields []CustomFieldConfig `json:"custom_fields,omitempty"`
}

type Woo struct {
	log  zerolog.Logger
	cfg  Config
	http *http.Client

	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	productLocks [64]sync.Mutex
	fatalOnce    sync.Once
	fatalCh      chan error
	circuitMu    sync.Mutex
	circuitFails int
	circuitUntil time.Time
}

func (w *Woo) Name() string { return "woocommerce" }

func (w *Woo) Start(ctx context.Context) error {
	w.ctx, w.cancel = context.WithCancel(ctx)
	if w.fatalCh == nil {
		w.fatalCh = make(chan error, 1)
	}
	w.log.Info().Str("integration", w.Name()).Msg("start")

	runtime, err := integrations.RuntimeFromContext(ctx)
	if err != nil {
		return fmt.Errorf("woocommerce: %w", err)
	}
	gdb := runtime.DB
	if gdb == nil {
		return fmt.Errorf("woocommerce: brak *gorm.DB w kontekście")
	}

	// 1) PRIME CACHE — jednorazowo przy starcie
	if w.cfg.Cache.PrimeOnStart {
		if err := w.primeCache(ctx, gdb); err != nil {
			return fmt.Errorf("woocommerce: prime cache: %w", err)
		}
	}
	runtime.MarkWooCacheReady()

	// Przywróć zadania przerwane przez poprzednie zamknięcie procesu, zanim
	// wystartują jakiekolwiek goroutines tej instancji integracji.
	if recovered, err := recoverRunningWooTasks(gdb); err != nil {
		return fmt.Errorf("woocommerce: recover running tasks: %w", err)
	} else if recovered > 0 {
		w.log.Warn().Int64("tasks", recovered).Msg("recovered interrupted Woo tasks")
	}

	if w.cfg.Cache.SweepIntervalMinutes > 0 {
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			w.runCacheSweeper(w.ctx, gdb)
		}()
	}

	// 2) odpal N workerów zadań
	for range w.numWorkers() {
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			w.runWorker(w.ctx, gdb)
		}()
	}

	for {
		select {
		case <-w.ctx.Done():
			w.wg.Wait()
			w.log.Info().Str("integration", w.Name()).Msg("stop")
			return nil
		case err := <-w.fatalCh:
			w.cancel()
			w.wg.Wait()
			return fmt.Errorf("woocommerce: fatal persistence error: %w", err)
		}
	}
}

func (w *Woo) recordFatal(err error) {
	if err == nil {
		return
	}
	w.fatalOnce.Do(func() {
		select {
		case w.fatalCh <- err:
		default:
		}
	})
}

func (w *Woo) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
}

func (w *Woo) numWorkers() int {
	if w.cfg.Workers > 0 {
		return w.cfg.Workers
	}
	return 3
}

func (w *Woo) interval() time.Duration {
	sec := w.cfg.PollSec
	if sec <= 0 {
		sec = 10
	}
	return time.Duration(sec) * time.Second
}

func factory(log zerolog.Logger, raw json.RawMessage) (integrations.Integration, error) {
	var cfg Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, err
	}
	base, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, fmt.Errorf("woocommerce: base_url must be an absolute http(s) URL")
	}
	if strings.TrimSpace(cfg.ConsumerKey) == "" || strings.TrimSpace(cfg.ConsumerSec) == "" ||
		strings.Contains(cfg.ConsumerKey, "xxx") || strings.Contains(cfg.ConsumerSec, "xxx") {
		return nil, fmt.Errorf("woocommerce: configure real consumer_key and consumer_secret")
	}
	if cfg.PollSec < 0 || cfg.Workers < 0 || cfg.Workers > 32 || cfg.Cache.SweepIntervalMinutes < 0 {
		return nil, fmt.Errorf("woocommerce: invalid negative interval or workers outside 0..32")
	}
	return &Woo{
		log:     log,
		cfg:     cfg,
		http:    &http.Client{Timeout: 15 * time.Second},
		fatalCh: make(chan error, 1),
	}, nil
}

func init() {
	integrations.Register("woocommerce", factory)
}
