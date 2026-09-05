package woocommerce

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestProductDecodesStringNumberAndEmptyPrices(t *testing.T) {
	for _, field := range []string{"regular_price", "sale_price", "hurt_price"} {
		for _, tc := range []struct{ name, raw, want string }{
			{"string", `"12.3400"`, "12.3400"},
			{"number", `12.34`, "12.34"},
			{"zero", `0`, "0"},
			{"exponent", `1.234e1`, "1.234e1"},
			{"precision", `123456789012345.6789`, "123456789012345.6789"},
			{"empty", `""`, ""},
			{"null", `null`, ""},
		} {
			t.Run(field+"/"+tc.name, func(t *testing.T) {
				raw := fmt.Sprintf(`{"id":301,"global_unique_id":"5901234567890",%q:%s,"extra_price":4.5,"meta_data":[{"key":"_example","value":7}]}`, field, tc.raw)
				var p wcProduct
				if err := json.Unmarshal([]byte(raw), &p); err != nil {
					t.Fatal(err)
				}
				if got := p.topLevelValue(field); got != tc.want {
					t.Fatalf("price got %q want %q", got, tc.want)
				}
				if p.ID != 301 || p.cacheEAN() != "5901234567890" || p.topLevelValue("extra_price") != "4.5" || p.metaValue("_example") != "7" {
					t.Fatalf("other product fields were lost: %+v", p)
				}
			})
		}
	}
}

func TestProductRejectsInvalidPriceTypesWithoutChangingReceiver(t *testing.T) {
	for _, field := range []string{"regular_price", "sale_price", "hurt_price"} {
		for _, value := range []string{`true`, `{}`, `[]`} {
			t.Run(field+"/"+value, func(t *testing.T) {
				p := wcProduct{ID: 99, SalePrice: "8"}
				err := json.Unmarshal([]byte(fmt.Sprintf(`{"id":301,%q:%s}`, field, value)), &p)
				if err == nil || !strings.Contains(err.Error(), field) {
					t.Fatalf("expected field-specific decode error, got %v", err)
				}
				if p.ID != 99 || p.SalePrice != "8" {
					t.Fatalf("failed decode modified receiver: %+v", p)
				}
			})
		}
	}
}

func TestProductMissingPricesClearPreviousValues(t *testing.T) {
	p := wcProduct{RegularPrice: "10", SalePrice: "5", HurtPrice: "3"}
	if err := json.Unmarshal([]byte(`{"id":301}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.RegularPrice != "" || p.SalePrice != "" || p.HurtPrice != "" {
		t.Fatalf("missing fields retained old prices: %+v", p)
	}
}
