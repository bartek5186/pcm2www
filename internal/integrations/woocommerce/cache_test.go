package woocommerce

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bartek5186/pcm2www/internal/db"
	"github.com/rs/zerolog"
)

func TestPrimeCacheStoresTaxClassAndCatalogVisibility(t *testing.T) {
	gdb := newWooWorkerTestDB(t)
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Query().Get("status") != "any" {
			t.Fatalf("prime must request every Woo status, got %q", r.URL.Query().Get("status"))
		}
		fields := r.URL.Query().Get("_fields")
		for _, required := range []string{"tax_class", "catalog_visibility"} {
			if !strings.Contains(fields, required) {
				t.Fatalf("request fields %q missing %q", fields, required)
			}
		}
		if requests == 1 {
			return jsonResponse(http.StatusOK, []wcProduct{{
				ID:                10,
				Name:              "Taxed product",
				TaxClass:          "800",
				CatalogVisibility: "catalog",
			}})
		}
		return jsonResponse(http.StatusOK, []wcProduct{})
	})}
	w := &Woo{log: zerolog.Nop(), cfg: Config{BaseURL: "https://woo.test"}, http: client}

	if err := w.primeCache(context.Background(), gdb); err != nil {
		t.Fatal(err)
	}

	var cached db.WooProductCache
	if err := gdb.Where("woo_id = ?", 10).Take(&cached).Error; err != nil {
		t.Fatal(err)
	}
	if cached.TaxClass != "800" || cached.CatalogVisibility != "catalog" {
		t.Fatalf("cache lost Woo fields: %+v", cached)
	}
}

func TestPrimeCacheRetainsPaginationMissWhenIndividualGETFindsProduct(t *testing.T) {
	gdb := newWooWorkerTestDB(t)
	if err := gdb.Create([]db.WooProductCache{{WooID: 10}, {WooID: 11}}).Error; err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/wp-json/wc/v3/products":
			return jsonResponse(http.StatusOK, []wcProduct{{ID: 10}})
		case "/wp-json/wc/v3/products/11":
			return jsonResponse(http.StatusOK, wcProduct{ID: 11, Name: "Moved during pagination"})
		default:
			return jsonResponse(http.StatusNotFound, map[string]any{})
		}
	})}
	w := &Woo{log: zerolog.Nop(), cfg: Config{BaseURL: "https://woo.test"}, http: client}
	if err := w.primeCache(context.Background(), gdb); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := gdb.Model(&db.WooProductCache{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("pagination miss deleted an existing product, cache count=%d", count)
	}
}

func TestSweepCacheUpdatesTaxClassAndCatalogVisibility(t *testing.T) {
	gdb := newWooWorkerTestDB(t)
	towarID := int64(101)
	if err := gdb.Create(&db.WooProductCache{
		WooID: 10, TowarID: &towarID, TaxClass: "2300", CatalogVisibility: "hidden",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := kvSetTime(gdb, "woo_cache_last_sweep", time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}

	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Query().Get("status") != "any" {
			t.Fatalf("sweep must request every Woo status, got %q", r.URL.Query().Get("status"))
		}
		if requests == 1 {
			return jsonResponse(http.StatusOK, []wcProduct{{
				ID: 10, TaxClass: "800", CatalogVisibility: "catalog",
				DateModifiedGMT: "2026-09-03T11:00:00",
			}})
		}
		return jsonResponse(http.StatusOK, []wcProduct{})
	})}
	w := &Woo{log: zerolog.Nop(), cfg: Config{BaseURL: "https://woo.test"}, http: client}
	w.sweepOnce(context.Background(), gdb)

	var cached db.WooProductCache
	if err := gdb.Where("woo_id = ?", 10).Take(&cached).Error; err != nil {
		t.Fatal(err)
	}
	if cached.TaxClass != "800" || cached.CatalogVisibility != "catalog" {
		t.Fatalf("sweep lost Woo fields: %+v", cached)
	}
	if cached.TowarID == nil || *cached.TowarID != towarID {
		t.Fatalf("sweep must preserve EAN link, got %+v", cached)
	}
}

func TestPrimeCacheRemovesProductsMissingFromWoo(t *testing.T) {
	gdb := newWooWorkerTestDB(t)
	if err := gdb.Create([]db.WooProductCache{
		{WooID: 10, Name: "Still exists"},
		{WooID: 11, Name: "Deleted in Woo"},
	}).Error; err != nil {
		t.Fatal(err)
	}

	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Path == "/wp-json/wc/v3/products/11" {
			return jsonResponse(http.StatusNotFound, map[string]any{"code": "woocommerce_rest_product_invalid_id"})
		}
		if r.URL.Query().Get("status") != "any" {
			t.Fatalf("full reconciliation must request every Woo status, got %q", r.URL.Query().Get("status"))
		}
		if requests == 1 {
			return jsonResponse(http.StatusOK, []wcProduct{{ID: 10, Name: "Still exists"}})
		}
		return jsonResponse(http.StatusOK, []wcProduct{})
	})}
	w := &Woo{log: zerolog.Nop(), cfg: Config{BaseURL: "https://woo.test"}, http: client}

	if err := w.primeCache(context.Background(), gdb); err != nil {
		t.Fatal(err)
	}
	var ids []uint
	if err := gdb.Model(&db.WooProductCache{}).Order("woo_id").Pluck("woo_id", &ids).Error; err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != 10 {
		t.Fatalf("stale cache product was not removed: %v", ids)
	}
}

func TestPrimeCacheAcceptsNumericPricesOnFourthPage(t *testing.T) {
	gdb := newWooWorkerTestDB(t)
	pages := 0
	w := &Woo{log: zerolog.Nop(), cfg: Config{BaseURL: "https://woo.test"}, http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		pages++
		if r.Method != http.MethodGet || r.URL.Query().Get("page") != strconv.Itoa(pages) {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
		}
		if pages > 4 {
			t.Fatal("prime did not stop after last page")
		}
		if pages == 4 {
			return jsonResponse(200, []map[string]any{{"id": 301, "global_unique_id": "5901234567890", "regular_price": 12.5, "sale_price": 9.5, "hurt_price": 7.5}})
		}
		products := make([]map[string]any, 100)
		for index := range products {
			products[index] = map[string]any{"id": (pages-1)*100 + index + 1, "regular_price": "10", "sale_price": "", "hurt_price": "8"}
		}
		return jsonResponse(200, products)
	})}}
	if err := w.primeCache(context.Background(), gdb); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := gdb.Model(&db.WooProductCache{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if pages != 4 || count != 301 {
		t.Fatalf("incomplete prime: pages=%d products=%d", pages, count)
	}
	var last db.WooProductCache
	if err := gdb.Where("woo_id = ?", 301).Take(&last).Error; err != nil {
		t.Fatal(err)
	}
	if last.PriceRegular != 12.5 || last.PriceSale != 9.5 || last.HurtPrice != 7.5 || last.Ean != "5901234567890" {
		t.Fatalf("numeric prices or EAN were lost: %+v", last)
	}
}
