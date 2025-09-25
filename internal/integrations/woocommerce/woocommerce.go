// internal/integrations/woocommerce/woocommerce.go
package woocommerce

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/bartek5186/pcm2www/internal/integrations"
	"github.com/rs/zerolog"
	"gorm.io/gorm"
)

type WooCache struct {
	PrimeOnStart          bool   `json:"prime_on_start"`
	SweepIntervalMinutes  int    `json:"sweep_interval_minutes"`
	SweepStockOnlyMinutes int    `json:"sweep_stock_only_minutes"`
	Fields                string `json:"fields"`
}

type Config struct {
	BaseURL     string   `json:"base_url"` // https://shop.example.com
	Username    string   `json:"username"` // opcjonalnie: dla opisu/logów albo Basic Auth
	ConsumerKey string   `json:"consumer_key"`
	ConsumerSec string   `json:"consumer_secret"`
	PollSec     int      `json:"poll_sec"` // co ile sekund sprawdzać (dev)
	Cache       WooCache `json:"cache"`
}

type Woo struct {
	log  zerolog.Logger
	cfg  Config
	http *http.Client

	ctx    context.Context
	cancel context.CancelFunc
}

func (w *Woo) Name() string { return "woocommerce" }

func (w *Woo) Start(ctx context.Context) error {
	w.ctx, w.cancel = context.WithCancel(ctx)
	w.log.Info().Str("integration", w.Name()).Msg("start")

	// weź *gorm.DB z kontekstu (tak, jak w importerze)
	raw := ctx.Value("gormDB")
	gdb, _ := raw.(*gorm.DB)
	if gdb == nil {
		return fmt.Errorf("woocommerce: brak *gorm.DB w kontekście")
	}

	// 1) PRIME CACHE — jednorazowo przy starcie
	if w.cfg.Cache.PrimeOnStart {
		if err := w.primeCache(ctx, gdb); err != nil {
			w.log.Error().Err(err).Msg("prime cache failed")
			// nie przerywam całej integracji – ale warto zalogować
		}
	}

	if w.cfg.Cache.SweepIntervalMinutes > 0 {
		go w.runCacheSweeper(w.ctx, gdb)
	}

	// 2) odpal worker zadań (jeśli już masz woo_tasks)
	//go w.runWorker(w.ctx, gdb)

	// 3) dev ping/ticker (jak miałeś)
	ticker := time.NewTicker(w.interval())
	defer ticker.Stop()
	w.tick()

	for {
		select {
		case <-w.ctx.Done():
			w.log.Info().Str("integration", w.Name()).Msg("stop")
			return nil
		case <-ticker.C:
			w.tick()
			ticker.Reset(w.interval())
		}
	}
}

func (w *Woo) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
}

func (w *Woo) interval() time.Duration {
	sec := w.cfg.PollSec
	if sec <= 0 {
		sec = 10
	}
	return time.Duration(sec) * time.Second
}

func (w *Woo) tick() {
	// DEV: zamiast prawdziwego API – „ping”
	w.log.Info().
		Str("integration", w.Name()).
		Str("shop", w.cfg.BaseURL).
		Msg("ping (dev) – tutaj pobierz np. /wp-json/wc/v3/orders?per_page=1")
}

func factory(log zerolog.Logger, raw json.RawMessage) (integrations.Integration, error) {
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	return &Woo{
		log:  log,
		cfg:  cfg,
		http: &http.Client{Timeout: 15 * time.Second},
	}, nil
}

func init() {
	integrations.Register("woocommerce", factory)
}
