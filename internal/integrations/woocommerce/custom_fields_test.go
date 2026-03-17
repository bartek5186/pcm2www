package woocommerce

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEnsureProductFieldsIncludesConfiguredCustomFields(t *testing.T) {
	fields := ensureProductFields("id,sku", []CustomFieldConfig{
		{
			Code:         "promo_price",
			ReadTopLevel: "promo_price",
			ReadMetaKey:  "_promo_price",
		},
	})

	for _, want := range []string{"promo_price", "meta_data", "regular_price", "global_unique_id"} {
		if !strings.Contains(fields, want) {
			t.Fatalf("expected %q in fields %q", want, fields)
		}
	}
}

func TestCustomFieldValueUsesConfiguredTopLevelAndMetaFallback(t *testing.T) {
	w := &Woo{
		cfg: Config{
			CustomFields: []CustomFieldConfig{
				{
					Code:         "promo_price",
					ReadTopLevel: "promo_price",
					ReadMetaKey:  "_promo_price",
				},
			},
		},
	}

	var topLevel wcProduct
	if err := json.Unmarshal([]byte(`{
		"id": 1,
		"promo_price": "12.34",
		"meta_data": [{"key":"_promo_price","value":"10.00"}]
	}`), &topLevel); err != nil {
		t.Fatal(err)
	}
	if got := w.customFieldValue(topLevel, "promo_price"); got != "12.34" {
		t.Fatalf("expected top-level custom value, got %q", got)
	}

	var metaOnly wcProduct
	if err := json.Unmarshal([]byte(`{
		"id": 2,
		"meta_data": [{"key":"_promo_price","value":"10.00"}]
	}`), &metaOnly); err != nil {
		t.Fatal(err)
	}
	if got := w.customFieldValue(metaOnly, "promo_price"); got != "10.00" {
		t.Fatalf("expected meta fallback value, got %q", got)
	}
}

func TestApplyCustomFieldPayloadWritesConfiguredTargets(t *testing.T) {
	w := &Woo{
		cfg: Config{
			CustomFields: []CustomFieldConfig{
				{
					Code:          "promo_price",
					WriteTopLevel: "promo_price",
					WriteMetaKey:  "_promo_price",
				},
			},
		},
	}

	body := map[string]any{}
	w.applyCustomFieldPayload(body, "promo_price", "12.34")

	if got := body["promo_price"]; got != "12.34" {
		t.Fatalf("expected top-level payload, got %#v", got)
	}
	meta, ok := body["meta_data"].([]map[string]any)
	if !ok || len(meta) != 1 {
		t.Fatalf("expected meta_data payload, got %#v", body["meta_data"])
	}
	if meta[0]["key"] != "_promo_price" || meta[0]["value"] != "12.34" {
		t.Fatalf("unexpected meta payload: %#v", meta[0])
	}
}
