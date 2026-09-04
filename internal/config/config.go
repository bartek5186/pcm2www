// internal/config/config.go
package conf

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bartek5186/pcm2www/internal/integrations/woocommerce"
)

type DBConfig struct {
	// Driver: sqlite | postgres | mysql
	Driver string `json:"driver"`
	// DSN używany dla postgres/mysql
	DSN string `json:"dsn,omitempty"`
	// Path używany dla sqlite (opcjonalny; domyślnie ~/.config/pcm2www/pcm2www.db)
	Path string `json:"path,omitempty"`
}

// Główny config aplikacji
type Config struct {
	Database            DBConfig                   `json:"database"`
	AutoStart           bool                       `json:"auto_start"`
	SyncIntervalSeconds int                        `json:"sync_interval_seconds"`
	Integrations        map[string]json.RawMessage `json:"integrations"` // nazwa -> surowy JSON integracji
	// (opcjonalnie, zostaw jeśli nadal używasz gdzieś indziej)
	WatchDir string `json:"watch_dir,omitempty"`
}

// Przykładowy config integracji WooCommerce (używany do domyślnego JSON-a)
type WooDefaults struct {
	BaseURL      string                          `json:"base_url"`
	ConsumerKey  string                          `json:"consumer_key"`
	ConsumerSec  string                          `json:"consumer_secret"`
	PollSec      int                             `json:"poll_sec"`
	Workers      int                             `json:"workers"`
	Cache        woocommerce.WooCache            `json:"cache"`
	CustomFields []woocommerce.CustomFieldConfig `json:"custom_fields,omitempty"`
}

type ImporterDefaults struct {
	WatchDir         string `json:"watch_dir"`
	PollSec          int    `json:"poll_sec"`
	PriceMode        string `json:"price_mode"`
	StabilitySeconds int    `json:"stability_seconds"`
}

func LoadOrCreate(path string) (*Config, bool, error) {
	// upewnij się, że katalog istnieje
	_ = os.MkdirAll(filepath.Dir(path), 0o755)

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Pierwszy plik jest kompletnym, ale nieaktywnym szablonem. AutoStart
			// pozostaje false, dopóki użytkownik nie wpisze danych Woo i nie
			// potwierdzi katalogu importu.
			woo := WooDefaults{
				BaseURL:     "https://example.com",
				ConsumerKey: "ck_xxx",
				ConsumerSec: "cs_xxx",
				PollSec:     10,
				Workers:     3,
				Cache: woocommerce.WooCache{
					PrimeOnStart:         true,
					SweepIntervalMinutes: 360, //6h
					Fields:               "id,sku,name,regular_price,sale_price,tax_class,stock_quantity,manage_stock,stock_status,backorders,catalog_visibility,status,date_modified_gmt,type,global_unique_id,ean",
				},
				CustomFields: []woocommerce.CustomFieldConfig{
					{
						Code:          "hurt_price",
						ReadTopLevel:  "hurt_price",
						ReadMetaKey:   "_hurt_price",
						WriteTopLevel: "hurt_price",
						WriteMetaKey:  "_hurt_price",
					},
				},
			}
			rawWoo, _ := json.Marshal(woo)
			rawImporter, _ := json.Marshal(ImporterDefaults{
				WatchDir:         "~/pcm2www/imports",
				PollSec:          5,
				PriceMode:        "gross",
				StabilitySeconds: 2,
			})

			cfg := &Config{
				Database: DBConfig{
					Driver: "sqlite",
				},
				AutoStart:           false,
				SyncIntervalSeconds: 5,
				Integrations: map[string]json.RawMessage{
					"woocommerce": rawWoo,
					"importer":    rawImporter,
				},
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
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, false, fmt.Errorf("błąd parsowania configa: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return nil, false, fmt.Errorf("błąd parsowania configa: wiele dokumentów JSON")
		}
		return nil, false, fmt.Errorf("błąd parsowania configa: %w", err)
	}
	if cfg.Integrations == nil {
		cfg.Integrations = map[string]json.RawMessage{}
	}
	if strings.TrimSpace(cfg.Database.Driver) == "" {
		cfg.Database.Driver = "sqlite"
	}
	if err := cfg.Validate(); err != nil {
		return nil, false, err
	}
	return &cfg, false, nil
}

func (c *Config) Validate() error {
	if c == nil {
		return fmt.Errorf("config jest pusty")
	}
	switch strings.ToLower(strings.TrimSpace(c.Database.Driver)) {
	case "sqlite", "postgres", "postgresql", "mysql":
	default:
		return fmt.Errorf("nieobsługiwany database.driver %q", c.Database.Driver)
	}
	if c.SyncIntervalSeconds < 0 {
		return fmt.Errorf("sync_interval_seconds nie może być ujemne")
	}
	if len(c.Integrations) == 0 {
		return fmt.Errorf("brak integracji w konfiguracji")
	}
	return nil
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
