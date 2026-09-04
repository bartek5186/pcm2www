package appsettings

import (
	"encoding/json"
	"testing"

	conf "github.com/bartek5186/pcm2www/internal/config"
	"github.com/bartek5186/pcm2www/internal/integrations/importer"
	"github.com/bartek5186/pcm2www/internal/integrations/woocommerce"
)

func TestApplyPreservesAdvancedAndUnrelatedConfiguration(t *testing.T) {
	woo := woocommerce.Config{
		BaseURL:     "https://old.example.com",
		ConsumerKey: "old-key",
		ConsumerSec: "old-secret",
		PollSec:     10,
		Workers:     3,
		Cache: woocommerce.WooCache{
			PrimeOnStart:         true,
			SweepIntervalMinutes: 360,
			Fields:               "id,ean,custom_field",
		},
		CustomFields: []woocommerce.CustomFieldConfig{{
			Code: "hurt_price", ReadMetaKey: "_hurt_price",
		}},
	}
	imp := importer.Config{WatchDir: "old", PollSec: 5, StabilitySeconds: 2, PriceMode: "gross"}
	rawWoo, _ := json.Marshal(woo)
	rawImporter, _ := json.Marshal(imp)
	base := &conf.Config{
		Database:            conf.DBConfig{Driver: "sqlite", Path: "unchanged.db"},
		AutoStart:           false,
		SyncIntervalSeconds: 17,
		Integrations: map[string]json.RawMessage{
			"woocommerce": rawWoo,
			"importer":    rawImporter,
			"other":       json.RawMessage(`{"enabled":true}`),
		},
	}

	updated, err := Apply(base, Values{
		AutoStart:               true,
		WooBaseURL:              " https://new.example.com ",
		WooConsumerKey:          " new-key ",
		WooConsumerSecret:       " new-secret ",
		WooPollSeconds:          4,
		WooWorkers:              7,
		WooPrimeOnStart:         false,
		WooSweepIntervalMinutes: 90,
		ImportWatchDir:          " C:/incoming ",
		ImportPollSeconds:       3,
		ImportStabilitySeconds:  1,
		ImportPriceMode:         " NET ",
	})
	if err != nil {
		t.Fatal(err)
	}

	if !updated.AutoStart || updated.Database != base.Database || updated.SyncIntervalSeconds != 17 {
		t.Fatalf("top-level configuration was not preserved: %+v", updated)
	}
	if string(updated.Integrations["other"]) != string(base.Integrations["other"]) {
		t.Fatal("unrelated integration configuration changed")
	}
	if base.AutoStart || string(base.Integrations["woocommerce"]) != string(rawWoo) {
		t.Fatal("Apply modified its input")
	}

	var gotWoo woocommerce.Config
	if err := updated.UnmarshalIntegration("woocommerce", &gotWoo); err != nil {
		t.Fatal(err)
	}
	if gotWoo.BaseURL != "https://new.example.com" || gotWoo.ConsumerKey != "new-key" || gotWoo.ConsumerSec != "new-secret" ||
		gotWoo.PollSec != 4 || gotWoo.Workers != 7 || gotWoo.Cache.PrimeOnStart || gotWoo.Cache.SweepIntervalMinutes != 90 {
		t.Fatalf("unexpected Woo settings: %+v", gotWoo)
	}
	if gotWoo.Cache.Fields != woo.Cache.Fields || len(gotWoo.CustomFields) != 1 || gotWoo.CustomFields[0].Code != "hurt_price" {
		t.Fatalf("advanced Woo settings were lost: %+v", gotWoo)
	}

	var gotImporter importer.Config
	if err := updated.UnmarshalIntegration("importer", &gotImporter); err != nil {
		t.Fatal(err)
	}
	if gotImporter.WatchDir != "C:/incoming" || gotImporter.PollSec != 3 || gotImporter.StabilitySeconds != 1 || gotImporter.PriceMode != "net" {
		t.Fatalf("unexpected importer settings: %+v", gotImporter)
	}
}

func TestFromConfigRejectsMissingRequiredIntegration(t *testing.T) {
	_, err := FromConfig(&conf.Config{Integrations: map[string]json.RawMessage{}})
	if err == nil {
		t.Fatal("missing integrations should be reported")
	}
}
