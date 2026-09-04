package conf

import (
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
