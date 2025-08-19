package conf

import (
	"encoding/json"
	"fmt"
	"os"
)

// WooCommerceConfig trzyma dane do integracji z WooCommerce
type WooCommerceConfig struct {
	Site string `json:"site"`
	User string `json:"user"`
	Key  string `json:"key"`
}

// Config to główna struktura konfiguracyjna
type Config struct {
	Integration         string            `json:"integration"` // np. "woocommerce"
	WooCommerce         WooCommerceConfig `json:"woocommerce"`
	AutoStart           bool              `json:"auto_start"`
	SyncIntervalSeconds int               `json:"sync_interval_seconds"`
}

// LoadOrCreate ładuje config z pliku lub tworzy domyślny
func LoadOrCreate(path string) (*Config, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg := &Config{
				Integration: "woocommerce",
				WooCommerce: WooCommerceConfig{
					Site: "https://example.com",
					User: "user",
					Key:  "key",
				},
				AutoStart:           false,
				SyncIntervalSeconds: 60,
			}
			if err := Save(path, cfg); err != nil {
				return nil, false, fmt.Errorf("błąd zapisu domyślnego configa: %w", err)
			}
			return cfg, true, nil
		}
		return nil, false, fmt.Errorf("błąd otwierania configa: %w", err)
	}
	defer f.Close()

	var cfg Config
	if err := json.NewDecoder(f).Decode(&cfg); err != nil {
		return nil, false, fmt.Errorf("błąd parsowania configa: %w", err)
	}

	return &cfg, false, nil
}

// Save zapisuje config do pliku
func Save(path string, cfg *Config) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(cfg)
}
