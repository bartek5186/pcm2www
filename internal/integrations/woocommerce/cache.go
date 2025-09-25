// internal/integrations/woocommerce/cache.go
package woocommerce

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
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
		q.Set("per_page", strconv.Itoa(perPage))
		q.Set("page", strconv.Itoa(page))
		q.Set("_fields", "id,sku,name,regular_price,sale_price,stock_quantity,manage_stock,status,hurt_price")

		base.RawQuery = q.Encode()

		req, err := http.NewRequestWithContext(ctx, "GET", base.String(), nil)
		if err != nil {
			return fmt.Errorf("error creating request: %w", err)
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "WpImporter 1.0v")
		req.SetBasicAuth(w.cfg.Username, w.cfg.ConsumerSec)

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

		/* Przykładowe dane
				    {
		        "id": 10816,
		        "sku": "000206",
		        "name": "Wino Donnafugata Mille e una Notte DOC",
		        "regular_price": "283.74",
		        "sale_price": "",
		        "stock_quantity": null,
		        "manage_stock": false,
		        "status": "publish",
		        "hurt_price": ""
		    },
		    {
		        "id": 10814,
		        "sku": "000203",
		        "name": "Wino Donnafugata Floramundi Cerasuolo di Vittoria DOCG",
		        "regular_price": "104.88",
		        "sale_price": "",
		        "stock_quantity": null,
		        "manage_stock": false,
		        "status": "publish",
		        "hurt_price": ""
		    },

		*/

		for _, p := range items {
			rows = append(rows, db.WooProductCache{
				WooID:        uint(p.ID),
				TowarID:      nil, // nie znamy jeszcze mapowania z PCM – zostanie uzupełnione później
				Kod:          p.SKU,
				Name:         p.Name,
				PriceRegular: parsePrice(p.RegularPrice),
				PriceSale:    parsePrice(p.SalePrice),
				HurtPrice:    parsePrice(p.HurtPrice),
				StockQty:     p.StockQty,
				StockManaged: p.ManageStock,
				Status:       p.Status,
			})
		}

		if err := gdb.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "woo_id"}}, // klucz unikalny
			DoUpdates: clause.AssignmentColumns([]string{
				"kod", "name", "price_regular", "price_sale", "hurt_price",
				"stock_qty", "stock_managed", "status",
			}),
		}).Create(&rows).Error; err != nil {
			return fmt.Errorf("upsert cache page %d: %w", page, err)
		}

		page++
	}

	w.log.Info().Msg("Woo cache primed (products)")
	return nil
}

// pomocniczo: Woo trzyma ceny jako string
func parsePrice(s string) float64 {
	if s == "" {
		return 0
	}
	v, _ := strconv.ParseFloat(s, 64)
	return v
}
