package conf

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFirstRunConfigIsCompleteAndDoesNotAutoStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, created, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("expected first-run config to be created")
	}
	if cfg.AutoStart {
		t.Fatal("first-run config must require an explicit start")
	}
	for _, name := range []string{"importer", "woocommerce"} {
		if _, ok := cfg.Integrations[name]; !ok {
			t.Fatalf("first-run config missing %s integration", name)
		}
	}
	var importer ImporterDefaults
	if err := cfg.UnmarshalIntegration("importer", &importer); err != nil {
		t.Fatal(err)
	}
	if importer.WatchDir == "" || importer.StabilitySeconds <= 0 {
		t.Fatalf("incomplete importer defaults: %+v", importer)
	}
}

func TestLoadRejectsUnknownConfigurationFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{"database":{"driver":"sqlite"},"integrations":{"importer":{}},"auto_start":false,"typo":true}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreate(path); err == nil {
		t.Fatal("unknown config field should be rejected")
	}
}

func TestLoadMigratesLegacyWatchDirToImporter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{
  "database": {"driver": "sqlite"},
  "auto_start": false,
  "sync_interval_seconds": 5,
  "integrations": {
    "woocommerce": {
      "base_url": "https://shop.example",
      "consumer_key": "ck_keep",
      "consumer_secret": "cs_keep",
      "workers": 7
    }
  },
  "watch_dir": "./xml_in"
}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, created, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("existing legacy config must not be reported as first-run")
	}
	var importer ImporterDefaults
	if err := cfg.UnmarshalIntegration("importer", &importer); err != nil {
		t.Fatal(err)
	}
	if importer.WatchDir != "./xml_in" || importer.PollSec != 5 || importer.PriceMode != "gross" || importer.StabilitySeconds != 2 {
		t.Fatalf("unexpected migrated importer config: %+v", importer)
	}

	var woo map[string]any
	if err := cfg.UnmarshalIntegration("woocommerce", &woo); err != nil {
		t.Fatal(err)
	}
	if woo["consumer_key"] != "ck_keep" || woo["workers"] != float64(7) {
		t.Fatalf("migration changed WooCommerce config: %#v", woo)
	}

	firstSave, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved Config
	if err := json.Unmarshal(firstSave, &saved); err != nil {
		t.Fatalf("migrated config was not saved as valid JSON: %v", err)
	}
	if _, ok := saved.Integrations["importer"]; !ok {
		t.Fatal("migrated importer section was not persisted")
	}

	if _, created, err := LoadOrCreate(path); err != nil {
		t.Fatal(err)
	} else if created {
		t.Fatal("second load must not be reported as first-run")
	}
	secondLoad, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstSave, secondLoad) {
		t.Fatal("migration is not idempotent: second load rewrote the config")
	}
}

func TestLoadMigrationDoesNotOverwriteExistingImporter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{
  "database": {"driver": "sqlite"},
  "integrations": {
    "woocommerce": {},
    "importer": {"watch_dir":"D:/pcm","poll_sec":9,"price_mode":"net","stability_seconds":4}
  },
  "watch_dir": "./legacy"
}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	var importer ImporterDefaults
	if err := cfg.UnmarshalIntegration("importer", &importer); err != nil {
		t.Fatal(err)
	}
	if importer.WatchDir != "D:/pcm" || importer.PollSec != 9 || importer.PriceMode != "net" || importer.StabilitySeconds != 4 {
		t.Fatalf("existing importer config was overwritten: %+v", importer)
	}
}

func TestLoadMigrationUsesDefaultImporterDirectoryWithoutLegacyWatchDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{"database":{"driver":"sqlite"},"integrations":{"woocommerce":{}}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	var importer ImporterDefaults
	if err := cfg.UnmarshalIntegration("importer", &importer); err != nil {
		t.Fatal(err)
	}
	if importer.WatchDir != defaultImporterWatchDir {
		t.Fatalf("expected default watch dir %q, got %q", defaultImporterWatchDir, importer.WatchDir)
	}
}
