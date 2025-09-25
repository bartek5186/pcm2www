// internal/config/config.go
package conf

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bartek5186/pcm2www/internal/integrations/woocommerce"
)

// Główny config aplikacji
type Config struct {
	AutoStart           bool                       `json:"auto_start"`
	SyncIntervalSeconds int                        `json:"sync_interval_seconds"`
	Integrations        map[string]json.RawMessage `json:"integrations"` // nazwa -> surowy JSON integracji
	// (opcjonalnie, zostaw jeśli nadal używasz gdzieś indziej)
	WatchDir string `json:"watch_dir,omitempty"`
}

// Przykładowy config integracji WooCommerce (używany do domyślnego JSON-a)
type WooDefaults struct {
	BaseURL     string               `json:"base_url"`
	Username    string               `json:"username"`
	ConsumerKey string               `json:"consumer_key"`
	ConsumerSec string               `json:"consumer_secret"`
	PollSec     int                  `json:"poll_sec"`
	Cache       woocommerce.WooCache `json:"cache"`
}

func LoadOrCreate(path string) (*Config, bool, error) {
	// upewnij się, że katalog istnieje
	_ = os.MkdirAll(filepath.Dir(path), 0o755)

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			// domyślny config
			woo := WooDefaults{
				BaseURL:     "https://example.com",
				Username:    "admin@example.com",
				ConsumerKey: "ck_xxx",
				ConsumerSec: "cs_xxx",
				PollSec:     10,
				Cache: woocommerce.WooCache{
					PrimeOnStart:          true,
					SweepIntervalMinutes:  360, //6h
					SweepStockOnlyMinutes: 120, //2h
					Fields:                "id,sku,name,regular_price,sale_price,stock_quantity,manage_stock,status,date_modified_gmt",
				},
			}
			rawWoo, _ := json.Marshal(woo)

			cfg := &Config{
				AutoStart:           false,
				SyncIntervalSeconds: 5,
				Integrations: map[string]json.RawMessage{
					"woocommerce": rawWoo,
				},
				WatchDir: "./xml_in", // jeśli nie używasz, możesz usunąć
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
	if cfg.Integrations == nil {
		cfg.Integrations = map[string]json.RawMessage{}
	}
	return &cfg, false, nil
}

func Save(path string, cfg *Config) error {
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(cfg)
}

// Helper do odczytu konkretnej integracji do struktury docelowej
func (c *Config) UnmarshalIntegration(name string, v any) error {
	raw, ok := c.Integrations[name]
	if !ok {
		return fmt.Errorf("brak integracji %q w configu", name)
	}
	return json.Unmarshal(raw, v)
}
