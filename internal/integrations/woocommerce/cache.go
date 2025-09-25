// internal/integrations/woocommerce/cache.go
package woocommerce

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bartek5186/pcm2www/internal/db"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (w *Woo) primeCache(ctx context.Context, gdb *gorm.DB) error {
	base, _ := url.Parse(w.cfg.BaseURL)
	// /wp-json/wc/v3/products with selected fields
	// hurt_price is top leveled custom field
	base.Path = "/wp-json/wc/v3/products"

	perPage := 100
	page := 1

	client := w.http
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}

	for {

		q := base.Query()
		q.Set("orderby", "modified")
		q.Set("order", "desc")
		q.Set("per_page", strconv.Itoa(perPage))
		q.Set("page", strconv.Itoa(page))

		q.Set("_fields", w.cfg.Cache.Fields)

		base.RawQuery = q.Encode()

		req, err := http.NewRequestWithContext(ctx, "GET", base.String(), nil)
		if err != nil {
			return fmt.Errorf("error creating request: %w", err)
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "PCM2WWW/1.0")
		req.SetBasicAuth(w.cfg.ConsumerKey, w.cfg.ConsumerSec)

		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("woo cache page %d: %w", page, err)
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			return fmt.Errorf("woo cache page %d: http %d", page, resp.StatusCode)
		}

		var items []wcProduct
		if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
			resp.Body.Close()
			return fmt.Errorf("decode page %d: %w", page, err)
		}
		resp.Body.Close()

		if len(items) == 0 {
			break
		}

		// upsert do woo_products_cache
		var rows []db.WooProductCache
		rows = make([]db.WooProductCache, 0, len(items))

		for _, p := range items {
			rows = append(rows, db.WooProductCache{
				WooID:        uint(p.ID),
				TowarID:      nil, // nie znamy jeszcze mapowania z PCM – zostanie uzupełnione później
				Kod:          p.SKU,
				Ean:          p.EAN,
				Name:         p.Name,
				PriceRegular: parsePrice(p.RegularPrice),
				PriceSale:    parsePrice(p.SalePrice),
				HurtPrice:    parsePrice(p.HurtPrice),
				StockQty:     p.StockQuantity,
				StockManaged: p.ManageStock,
				Status:       p.Status,
				Type:         p.Type,
				DateModified: p.DateModifiedGMT,
			})
		}

		if err := gdb.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "woo_id"}}, // klucz unikalny
			DoUpdates: clause.AssignmentColumns([]string{
				"kod", "name", "price_regular", "price_sale", "hurt_price",
				"stock_qty", "stock_managed", "status", "ean", "type", "date_modified",
			}),
		}).Create(&rows).Error; err != nil {
			return fmt.Errorf("upsert cache page %d: %w", page, err)
		}

		page++
	}

	w.log.Info().Msg("Woo cache primed (products)")
	return nil
}

func (w *Woo) runCacheSweeper(ctx context.Context, gdb *gorm.DB) {
	intv := time.Duration(w.cfg.Cache.SweepIntervalMinutes) * time.Minute
	if intv <= 0 {
		w.log.Info().Msg("cache sweeper disabled (interval <= 0)")
		return
	}
	// pierwszy przelot zaraz po starcie
	w.sweepOnce(ctx, gdb)

	ticker := time.NewTicker(intv)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.sweepOnce(ctx, gdb)
		}
	}
}

func (w *Woo) sweepOnce(ctx context.Context, gdb *gorm.DB) {
	const kvKey = "woo_cache_last_sweep"
	last, ok := kvGetTime(gdb, kvKey)
	if !ok {
		// pierwszy raz: cofamy się o 24h, żeby nie ciągnąć całego sklepu
		last = time.Now().UTC().Add(-24 * time.Hour)
	}

	base, _ := url.Parse(w.cfg.BaseURL)
	base.Path = "/wp-json/wc/v3/products"

	perPage := 100
	page := 1

	client := w.http
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}

	total := 0
	var newest time.Time

	for {
		q := base.Query()
		q.Set("orderby", "modified")
		q.Set("order", "desc")
		q.Set("per_page", strconv.Itoa(perPage))
		q.Set("page", strconv.Itoa(page))
		// pobieramy wyłącznie potrzebne pola
		fields := w.cfg.Cache.Fields
		if strings.TrimSpace(fields) == "" {
			fields = "id,sku,name,regular_price,sale_price,stock_quantity,manage_stock,status,date_modified_gmt,type,hurt_price,ean"
		}
		q.Set("_fields", fields)
		base.RawQuery = q.Encode()

		req, err := http.NewRequestWithContext(ctx, "GET", base.String(), nil)
		if err != nil {
			w.log.Error().Err(err).Msg("sweep: build request")
			return
		}
		req.SetBasicAuth(w.cfg.ConsumerKey, w.cfg.ConsumerSec)

		req.Header.Set("User-Agent", "PCM2WWW/1.0")

		resp, err := client.Do(req)
		if err != nil {
			w.log.Error().Err(err).Int("page", page).Msg("sweep request failed")
			return
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			w.log.Error().Int("code", resp.StatusCode).Int("page", page).Msg("sweep http error")
			return
		}

		var items []wcProduct
		if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
			resp.Body.Close()
			w.log.Error().Err(err).Int("page", page).Msg("sweep decode failed")
			return
		}
		resp.Body.Close()

		if len(items) == 0 {
			break
		}

		rows := make([]db.WooProductCache, 0, len(items))
		stop := false

		for _, p := range items {

			tm, err := parseWooTimeUTC(p.DateModifiedGMT)
			if err != nil {
				// brak parsowania → zaloguj i pomiń
				w.log.Debug().Str("date_modified_gmt", p.DateModifiedGMT).
					Msg("sweep: cannot parse date_modified_gmt")
				continue
			}

			if newest.IsZero() || tm.After(newest) {
				newest = tm
			}

			if !tm.After(last) { // (<= last) → reszta będzie starsza (sort=desc)
				stop = true
				break
			}

			rows = append(rows, db.WooProductCache{
				WooID:        uint(p.ID),
				TowarID:      nil,
				Kod:          p.SKU,
				Ean:          p.EAN,
				Name:         p.Name,
				PriceRegular: parsePrice(p.RegularPrice),
				PriceSale:    parsePrice(p.SalePrice),
				HurtPrice:    parsePrice(p.HurtPrice),
				StockQty:     p.StockQuantity,
				StockManaged: p.ManageStock,
				Status:       p.Status,
				Type:         p.Type,
				DateModified: p.DateModifiedGMT,
			})
		}

		if len(rows) > 0 {
			if err := gdb.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "woo_id"}},
				DoUpdates: clause.AssignmentColumns([]string{
					"kod", "ean", "name", "price_regular", "price_sale", "hurt_price",
					"stock_qty", "stock_managed", "status", "type", "date_modified",
				}),
			}).Create(&rows).Error; err != nil {
				w.log.Error().Err(err).Msg("sweep upsert failed")
				return
			}
			total += len(rows)
		}

		if stop {
			break
		}
		page++
	}

	// tylko gdy coś realnie przetworzono
	// i ustaw na "najnowszy widziany tm", nie "teraz"
	if total > 0 && !newest.IsZero() {
		if err := kvSetTime(gdb, kvKey, newest); err != nil {
			w.log.Error().Err(err).Msg("kvSetTime failed")
		}
		w.log.Info().Int("upserts", total).Time("since", last).Time("newest", newest).Msg("cache sweep done")
	} else {
		w.log.Debug().Time("since", last).Msg("cache sweep done (no changes)")
	}

}

func parsePrice(s string) float64 {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", "."))
	if s == "" {
		return 0
	}
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

func kvGetTime(gdb *gorm.DB, key string) (time.Time, bool) {
	var row db.KV
	if err := gdb.Where("k = ?", key).Take(&row).Error; err != nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, row.V)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func kvSetTime(gdb *gorm.DB, key string, t time.Time) error {
	row := db.KV{K: key, V: t.UTC().Format(time.RFC3339)}
	return gdb.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "k"}},
		DoUpdates: clause.AssignmentColumns([]string{"v"}),
	}).Create(&row).Error
}

// dodaj helper na górze pliku
func parseWooTimeUTC(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty time")
	}
	// spróbuj standardów z TZ
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), nil
	}
	// Woo często zwraca bez TZ, np. "2006-01-02T15:04:05[.999999]"
	// potraktuj jako UTC
	layouts := []string{
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05.999999",
		"2006-01-02T15:04:05.999",
		"2006-01-02T15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported time format: %q", s)
}
