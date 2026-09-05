package appsettings

import (
	"encoding/json"
	"fmt"
	"strings"

	conf "github.com/bartek5186/pcm2www/internal/config"
	"github.com/bartek5186/pcm2www/internal/integrations/importer"
	"github.com/bartek5186/pcm2www/internal/integrations/woocommerce"
)

// Values contains the part of config.json edited by the Windows settings UI.
// Fields not represented here (database and custom-field mappings) are preserved.
type Values struct {
	AutoStart     bool
	WooEnabled    bool
	ImportEnabled bool

	WooBaseURL              string
	WooConsumerKey          string
	WooConsumerSecret       string
	WooPollSeconds          int
	WooWorkers              int
	WooPrimeOnStart         bool
	WooSweepIntervalMinutes int

	ImportWatchDir         string
	ImportPollSeconds      int
	ImportStabilitySeconds int
	ImportPriceMode        string
}

func FromConfig(cfg *conf.Config) (Values, error) {
	if cfg == nil {
		return Values{}, fmt.Errorf("konfiguracja jest pusta")
	}

	var woo woocommerce.Config
	if err := unmarshalOptional(cfg, "woocommerce", &woo); err != nil {
		return Values{}, err
	}
	var imp importer.Config
	if err := unmarshalOptional(cfg, "importer", &imp); err != nil {
		return Values{}, err
	}

	priceMode := strings.ToLower(strings.TrimSpace(imp.PriceMode))
	if priceMode == "" {
		priceMode = "gross"
	}

	wooEnabled, err := cfg.IntegrationEnabled("woocommerce")
	if err != nil {
		return Values{}, err
	}
	importEnabled, err := cfg.IntegrationEnabled("importer")
	if err != nil {
		return Values{}, err
	}
	return Values{
		AutoStart:     cfg.AutoStart,
		WooEnabled:    wooEnabled,
		ImportEnabled: importEnabled,

		WooBaseURL:              woo.BaseURL,
		WooConsumerKey:          woo.ConsumerKey,
		WooConsumerSecret:       woo.ConsumerSec,
		WooPollSeconds:          woo.PollSec,
		WooWorkers:              woo.Workers,
		WooPrimeOnStart:         woo.Cache.PrimeOnStart,
		WooSweepIntervalMinutes: woo.Cache.SweepIntervalMinutes,

		ImportWatchDir:         imp.WatchDir,
		ImportPollSeconds:      imp.PollSec,
		ImportStabilitySeconds: imp.StabilitySeconds,
		ImportPriceMode:        priceMode,
	}, nil
}

func Apply(base *conf.Config, values Values) (*conf.Config, error) {
	if base == nil {
		return nil, fmt.Errorf("konfiguracja jest pusta")
	}

	var woo woocommerce.Config
	if err := unmarshalOptional(base, "woocommerce", &woo); err != nil {
		return nil, err
	}
	var imp importer.Config
	if err := unmarshalOptional(base, "importer", &imp); err != nil {
		return nil, err
	}

	woo.Enabled = &values.WooEnabled
	imp.Enabled = &values.ImportEnabled
	woo.BaseURL = strings.TrimSpace(values.WooBaseURL)
	woo.ConsumerKey = strings.TrimSpace(values.WooConsumerKey)
	woo.ConsumerSec = strings.TrimSpace(values.WooConsumerSecret)
	woo.PollSec = values.WooPollSeconds
	woo.Workers = values.WooWorkers
	woo.Cache.PrimeOnStart = values.WooPrimeOnStart
	woo.Cache.SweepIntervalMinutes = values.WooSweepIntervalMinutes

	imp.WatchDir = strings.TrimSpace(values.ImportWatchDir)
	imp.PollSec = values.ImportPollSeconds
	imp.StabilitySeconds = values.ImportStabilitySeconds
	imp.PriceMode = strings.ToLower(strings.TrimSpace(values.ImportPriceMode))

	rawWoo, err := json.Marshal(woo)
	if err != nil {
		return nil, fmt.Errorf("kodowanie ustawień WooCommerce: %w", err)
	}
	rawImporter, err := json.Marshal(imp)
	if err != nil {
		return nil, fmt.Errorf("kodowanie ustawień importera: %w", err)
	}

	updated := *base
	updated.AutoStart = values.AutoStart
	updated.Integrations = make(map[string]json.RawMessage, len(base.Integrations))
	for name, raw := range base.Integrations {
		updated.Integrations[name] = append(json.RawMessage(nil), raw...)
	}
	updated.Integrations["woocommerce"] = rawWoo
	updated.Integrations["importer"] = rawImporter
	if err := updated.Validate(); err != nil {
		return nil, err
	}
	return &updated, nil
}

func unmarshalOptional(cfg *conf.Config, name string, dst any) error {
	if _, ok := cfg.Integrations[name]; !ok {
		return nil
	}
	return cfg.UnmarshalIntegration(name, dst)
}
