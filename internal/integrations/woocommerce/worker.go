package woocommerce

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bartek5186/pcm2www/internal/db"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (w *Woo) runWorker(ctx context.Context, gdb *gorm.DB) {
	ticker := time.NewTicker(w.interval())
	defer ticker.Stop()

	w.workerTick(ctx, gdb)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.workerTick(ctx, gdb)
		}
	}
}

// batchableKinds to typy tasków obsługiwane przez batch GET+POST.
var batchableKinds = []string{db.WooTaskKindPriceUpdate, db.WooTaskKindStockUpdate}

const workerBatchSize = 20

func (w *Woo) workerTick(ctx context.Context, gdb *gorm.DB) {
	for {
		if ctx.Err() != nil {
			return
		}

		// 1) Spróbuj batch dla każdego batchable kind
		didBatch := false
		for _, kind := range batchableKinds {
			tasks, err := claimNextNWooTasksOfKind(gdb, kind, workerBatchSize)
			if err != nil {
				w.log.Error().Err(err).Str("kind", kind).Msg("woo worker: claim batch failed")
				return
			}
			if len(tasks) == 0 {
				continue
			}
			w.executeBatch(ctx, gdb, kind, tasks)
			didBatch = true
			break
		}
		if didBatch {
			continue
		}

		// 2) Pozostałe typy (ean.update, availability.update) — sekwencyjnie
		task, err := claimNextSequentialWooTask(gdb)
		if err != nil {
			w.log.Error().Err(err).Msg("woo worker: claim task failed")
			return
		}
		if task == nil {
			return
		}
		w.executeWooTask(ctx, gdb, *task)
		if ctx.Err() != nil {
			return
		}
	}
}

// claimNextNWooTasksOfKind atomicznie claim-uje do n tasków danego kind.
func claimNextNWooTasksOfKind(gdb *gorm.DB, kind string, n int) ([]db.WooTask, error) {
	var claimed []db.WooTask
	for range n {
		task, err := claimOneWooTaskOfKind(gdb, kind)
		if err != nil {
			return claimed, err
		}
		if task == nil {
			break
		}
		claimed = append(claimed, *task)
	}
	return claimed, nil
}

func claimOneWooTaskOfKind(gdb *gorm.DB, kind string) (*db.WooTask, error) {
	var tasks []db.WooTask
	if err := gdb.
		Where("status = ? AND kind = ?", "pending", kind).
		Order("created_at ASC, task_id ASC").
		Limit(1).
		Find(&tasks).Error; err != nil {
		return nil, err
	}
	if len(tasks) == 0 {
		return nil, nil
	}
	return claimWooTask(gdb, tasks[0])
}

// claimNextSequentialWooTask claim-uje jeden task spoza batchableKinds.
func claimNextSequentialWooTask(gdb *gorm.DB) (*db.WooTask, error) {
	var tasks []db.WooTask
	if err := gdb.
		Where("status = ? AND kind NOT IN ?", "pending", batchableKinds).
		Order("created_at ASC, task_id ASC").
		Limit(1).
		Find(&tasks).Error; err != nil {
		return nil, err
	}
	if len(tasks) == 0 {
		return nil, nil
	}
	return claimWooTask(gdb, tasks[0])
}

func claimWooTask(gdb *gorm.DB, task db.WooTask) (*db.WooTask, error) {
	now := time.Now()
	res := gdb.Model(&db.WooTask{}).
		Where("task_id = ? AND status = ?", task.TaskID, "pending").
		Updates(map[string]any{
			"status":     "running",
			"started_at": now,
			"attempts":   gorm.Expr("attempts + 1"),
			"last_error": "",
		})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, nil
	}
	task.Status = "running"
	task.StartedAt = &now
	task.Attempts++
	return &task, nil
}

func (w *Woo) executeWooTask(ctx context.Context, gdb *gorm.DB, task db.WooTask) {
	w.log.Info().
		Uint("task_id", task.TaskID).
		Uint("import_id", task.ImportID).
		Str("kind", task.Kind).
		Interface("woo_id", task.WooID).
		Interface("towar_id", task.TowarID).
		Msg("woo worker: processing task")

	switch task.Kind {
	case db.WooTaskKindEANUpdate:
		var payload db.WooEANUpdatePayload
		if err := json.Unmarshal([]byte(task.PayloadJSON), &payload); err != nil {
			w.failWooTask(gdb, task, fmt.Errorf("decode ean payload: %w", err))
			return
		}
		w.handleEANUpdate(ctx, gdb, task, payload)

	case db.WooTaskKindStockUpdate:
		var payload db.WooStockUpdatePayload
		if err := json.Unmarshal([]byte(task.PayloadJSON), &payload); err != nil {
			w.failWooTask(gdb, task, fmt.Errorf("decode stock payload: %w", err))
			return
		}
		w.handleStockUpdate(ctx, gdb, task, payload)

	case db.WooTaskKindPriceUpdate:
		var payload db.WooPriceUpdatePayload
		if err := json.Unmarshal([]byte(task.PayloadJSON), &payload); err != nil {
			w.failWooTask(gdb, task, fmt.Errorf("decode price payload: %w", err))
			return
		}
		w.handlePriceUpdate(ctx, gdb, task, payload)

	case db.WooTaskKindAvailabilityUpdate:
		var payload db.WooAvailabilityPayload
		if err := json.Unmarshal([]byte(task.PayloadJSON), &payload); err != nil {
			w.failWooTask(gdb, task, fmt.Errorf("decode availability payload: %w", err))
			return
		}
		w.handleAvailabilityUpdate(ctx, gdb, task, payload)

	default:
		w.failWooTask(gdb, task, fmt.Errorf("unsupported task kind: %s", task.Kind))
	}
}

func (w *Woo) handleEANUpdate(ctx context.Context, gdb *gorm.DB, task db.WooTask, payload db.WooEANUpdatePayload) {
	product, err := w.fetchProduct(ctx, payload.WooID)
	if err != nil {
		w.failWooTask(gdb, task, fmt.Errorf("fetch live product before ean update: %w", err))
		return
	}

	switch {
	case product.cacheEAN() == payload.DesiredEAN:
		if err := w.syncCacheFromVerifiedProduct(gdb, product, payload.TowarID); err != nil {
			w.failWooTask(gdb, task, fmt.Errorf("cache sync after already-set ean: %w", err))
			return
		}
		w.completeWooTask(gdb, task, "done", "", product.cacheEAN())
		w.log.Info().
			Uint("task_id", task.TaskID).
			Uint("import_id", task.ImportID).
			Uint("woo_id", payload.WooID).
			Str("ean", payload.DesiredEAN).
			Msg("woo worker: EAN already set and verified")
		w.logImportBatchStatus(gdb, task.ImportID)
		return

	case product.cacheEAN() != "":
		if err := w.syncCacheFromVerifiedProduct(gdb, product, payload.TowarID); err != nil {
			w.failWooTask(gdb, task, fmt.Errorf("cache sync after ean policy skip: %w", err))
			return
		}
		msg := fmt.Sprintf("policy skip: product already has live EAN %s", product.cacheEAN())
		w.completeWooTask(gdb, task, "skipped", msg, product.cacheEAN())
		w.log.Warn().
			Uint("task_id", task.TaskID).
			Uint("import_id", task.ImportID).
			Uint("woo_id", payload.WooID).
			Str("live_ean", product.cacheEAN()).
			Str("desired_ean", payload.DesiredEAN).
			Msg("woo worker: skip EAN overwrite by policy")
		w.logImportBatchStatus(gdb, task.ImportID)
		return
	}

	var duplicateOwners []uint
	if err := gdb.Model(&db.WooProductCache{}).
		Where("woo_id <> ? AND ean = ?", payload.WooID, payload.DesiredEAN).
		Pluck("woo_id", &duplicateOwners).Error; err != nil {
		w.failWooTask(gdb, task, fmt.Errorf("check duplicate ean in cache: %w", err))
		return
	}
	if len(duplicateOwners) > 0 {
		msg := fmt.Sprintf("policy skip: desired EAN already present in cache on Woo IDs %v", duplicateOwners)
		w.completeWooTask(gdb, task, "skipped", msg, "")
		w.log.Warn().
			Uint("task_id", task.TaskID).
			Uint("import_id", task.ImportID).
			Uint("woo_id", payload.WooID).
			Str("desired_ean", payload.DesiredEAN).
			Interface("owners", duplicateOwners).
			Msg("woo worker: skip duplicate EAN by policy")
		w.logImportBatchStatus(gdb, task.ImportID)
		return
	}

	verified, err := w.updateAndVerifyProduct(ctx, payload.WooID, map[string]any{
		"global_unique_id": payload.DesiredEAN,
	})
	if err != nil {
		if strings.Contains(err.Error(), "product_invalid_global_unique_id") {
			w.completeWooTask(gdb, task, "skipped", err.Error(), "")
			w.log.Warn().
				Uint("task_id", task.TaskID).
				Uint("import_id", task.ImportID).
				Uint("woo_id", payload.WooID).
				Str("desired_ean", payload.DesiredEAN).
				Err(err).
				Msg("woo worker: EAN rejected by Woo policy")
			w.logImportBatchStatus(gdb, task.ImportID)
			return
		}
		w.failWooTask(gdb, task, fmt.Errorf("update ean: %w", err))
		return
	}
	if verified.cacheEAN() != payload.DesiredEAN {
		w.failWooTask(gdb, task, fmt.Errorf("ean verification mismatch: got %q want %q", verified.cacheEAN(), payload.DesiredEAN))
		return
	}
	if err := w.syncCacheFromVerifiedProduct(gdb, verified, payload.TowarID); err != nil {
		w.failWooTask(gdb, task, fmt.Errorf("cache sync after ean update: %w", err))
		return
	}
	w.completeWooTask(gdb, task, "done", "", verified.cacheEAN())
	w.log.Info().
		Uint("task_id", task.TaskID).
		Uint("import_id", task.ImportID).
		Uint("woo_id", payload.WooID).
		Str("verified_ean", verified.cacheEAN()).
		Msg("woo worker: EAN updated and verified")
	w.logImportBatchStatus(gdb, task.ImportID)
}

func (w *Woo) handleStockUpdate(ctx context.Context, gdb *gorm.DB, task db.WooTask, payload db.WooStockUpdatePayload) {
	product, err := w.fetchProduct(ctx, payload.WooID)
	if err != nil {
		w.failWooTask(gdb, task, fmt.Errorf("fetch live product before stock update: %w", err))
		return
	}

	switch {
	case !product.ManageStock:
		if err := w.syncCacheFromVerifiedProduct(gdb, product, payload.TowarID); err != nil {
			w.failWooTask(gdb, task, fmt.Errorf("cache sync after stock policy skip: %w", err))
			return
		}
		msg := "policy skip: live product has manage_stock=false"
		w.completeWooTask(gdb, task, "skipped", msg, "")
		w.log.Warn().
			Uint("task_id", task.TaskID).
			Uint("import_id", task.ImportID).
			Uint("woo_id", payload.WooID).
			Float64("desired_stock", payload.DesiredStock).
			Msg("woo worker: skip stock update because manage_stock is false")
		w.logImportBatchStatus(gdb, task.ImportID)
		return

	case floatAlmostEqual(product.StockQuantity, payload.DesiredStock):
		if err := w.syncCacheFromVerifiedProduct(gdb, product, payload.TowarID); err != nil {
			w.failWooTask(gdb, task, fmt.Errorf("cache sync after already-set stock: %w", err))
			return
		}
		w.completeWooTask(gdb, task, "done", "", "")
		w.log.Info().
			Uint("task_id", task.TaskID).
			Uint("import_id", task.ImportID).
			Uint("woo_id", payload.WooID).
			Float64("stock", payload.DesiredStock).
			Msg("woo worker: stock already set and verified")
		w.logImportBatchStatus(gdb, task.ImportID)
		return
	}

	verified, err := w.updateAndVerifyProduct(ctx, payload.WooID, map[string]any{
		"stock_quantity": payload.DesiredStock,
	})
	if err != nil {
		w.failWooTask(gdb, task, fmt.Errorf("update stock: %w", err))
		return
	}
	if !floatAlmostEqual(verified.StockQuantity, payload.DesiredStock) {
		w.failWooTask(gdb, task, fmt.Errorf("stock verification mismatch: got %v want %v", verified.StockQuantity, payload.DesiredStock))
		return
	}
	if err := w.syncCacheFromVerifiedProduct(gdb, verified, payload.TowarID); err != nil {
		w.failWooTask(gdb, task, fmt.Errorf("cache sync after stock update: %w", err))
		return
	}
	w.completeWooTask(gdb, task, "done", "", "")
	w.log.Info().
		Uint("task_id", task.TaskID).
		Uint("import_id", task.ImportID).
		Uint("woo_id", payload.WooID).
		Float64("verified_stock", verified.StockQuantity).
		Msg("woo worker: stock updated and verified")
	w.logImportBatchStatus(gdb, task.ImportID)
}

func (w *Woo) handlePriceUpdate(ctx context.Context, gdb *gorm.DB, task db.WooTask, payload db.WooPriceUpdatePayload) {
	product, err := w.fetchProduct(ctx, payload.WooID)
	if err != nil {
		w.failWooTask(gdb, task, fmt.Errorf("fetch live product before price update: %w", err))
		return
	}

	switch {
	case parsePrice(product.SalePrice) > 0:
		if err := w.syncCacheFromVerifiedProduct(gdb, product, payload.TowarID); err != nil {
			w.failWooTask(gdb, task, fmt.Errorf("cache sync after price policy skip: %w", err))
			return
		}
		msg := fmt.Sprintf("policy skip: live sale_price=%v", product.SalePrice)
		w.completeWooTask(gdb, task, "skipped", msg, "")
		w.log.Warn().
			Uint("task_id", task.TaskID).
			Uint("import_id", task.ImportID).
			Uint("woo_id", payload.WooID).
			Float64("live_sale_price", parsePrice(product.SalePrice)).
			Msg("woo worker: skip price update because sale price is active")
		w.logImportBatchStatus(gdb, task.ImportID)
		return

	case floatAlmostEqual(parsePrice(product.RegularPrice), payload.DesiredRegular) &&
		floatAlmostEqual(parsePrice(w.customFieldValue(product, "hurt_price")), payload.DesiredHurt) &&
		product.TaxClass == payload.DesiredTaxClass:
		if err := w.syncCacheFromVerifiedProduct(gdb, product, payload.TowarID); err != nil {
			w.failWooTask(gdb, task, fmt.Errorf("cache sync after already-set price: %w", err))
			return
		}
		w.completeWooTask(gdb, task, "done", "", "")
		w.log.Info().
			Uint("task_id", task.TaskID).
			Uint("import_id", task.ImportID).
			Uint("woo_id", payload.WooID).
			Float64("regular_price", payload.DesiredRegular).
			Float64("hurt_price", payload.DesiredHurt).
			Str("tax_class", payload.DesiredTaxClass).
			Msg("woo worker: price already set and verified")
		w.logImportBatchStatus(gdb, task.ImportID)
		return
	}

	body := map[string]any{
		"regular_price": formatWooPrice(payload.DesiredRegular),
		"tax_class":     payload.DesiredTaxClass,
	}
	w.applyCustomFieldPayload(body, "hurt_price", formatWooPrice(payload.DesiredHurt))

	verified, err := w.updateAndVerifyProduct(ctx, payload.WooID, body)
	if err != nil {
		w.failWooTask(gdb, task, fmt.Errorf("update price: %w", err))
		return
	}
	if !floatAlmostEqual(parsePrice(verified.RegularPrice), payload.DesiredRegular) ||
		!floatAlmostEqual(parsePrice(w.customFieldValue(verified, "hurt_price")), payload.DesiredHurt) ||
		verified.TaxClass != payload.DesiredTaxClass {
		w.failWooTask(gdb, task, fmt.Errorf(
			"price verification mismatch: got regular=%v hurt=%v tax_class=%v want regular=%v hurt=%v tax_class=%v",
			parsePrice(verified.RegularPrice), parsePrice(w.customFieldValue(verified, "hurt_price")), verified.TaxClass,
			payload.DesiredRegular, payload.DesiredHurt, payload.DesiredTaxClass,
		))
		return
	}
	if err := w.syncCacheFromVerifiedProduct(gdb, verified, payload.TowarID); err != nil {
		w.failWooTask(gdb, task, fmt.Errorf("cache sync after price update: %w", err))
		return
	}
	w.completeWooTask(gdb, task, "done", "", "")
	w.log.Info().
		Uint("task_id", task.TaskID).
		Uint("import_id", task.ImportID).
		Uint("woo_id", payload.WooID).
		Float64("verified_regular", parsePrice(verified.RegularPrice)).
		Float64("verified_hurt", parsePrice(w.customFieldValue(verified, "hurt_price"))).
		Str("verified_tax_class", verified.TaxClass).
		Msg("woo worker: price updated and verified")
	w.logImportBatchStatus(gdb, task.ImportID)
}

func (w *Woo) handleAvailabilityUpdate(ctx context.Context, gdb *gorm.DB, task db.WooTask, payload db.WooAvailabilityPayload) {
	product, err := w.fetchProduct(ctx, payload.WooID)
	if err != nil {
		w.failWooTask(gdb, task, fmt.Errorf("fetch live product before availability update: %w", err))
		return
	}

	if payload.Unavailable {
		if !product.ManageStock && product.StockStatus == "outofstock" {
			if err := w.syncCacheFromVerifiedProduct(gdb, product, payload.TowarID); err != nil {
				w.failWooTask(gdb, task, fmt.Errorf("cache sync after already-set unavailable: %w", err))
				return
			}
			w.completeWooTask(gdb, task, "done", "", "")
			w.log.Info().Uint("task_id", task.TaskID).Uint("woo_id", payload.WooID).
				Msg("woo worker: product already unavailable and verified")
			w.logImportBatchStatus(gdb, task.ImportID)
			return
		}
		verified, err := w.updateAndVerifyProduct(ctx, payload.WooID, map[string]any{
			"manage_stock": false,
			"stock_status": "outofstock",
		})
		if err != nil {
			w.failWooTask(gdb, task, fmt.Errorf("update availability (unavailable): %w", err))
			return
		}
		if verified.ManageStock || verified.StockStatus != "outofstock" {
			w.failWooTask(gdb, task, fmt.Errorf("availability verification mismatch: got manage_stock=%v stock_status=%q", verified.ManageStock, verified.StockStatus))
			return
		}
		if err := w.syncCacheFromVerifiedProduct(gdb, verified, payload.TowarID); err != nil {
			w.failWooTask(gdb, task, fmt.Errorf("cache sync after unavailable update: %w", err))
			return
		}
		w.completeWooTask(gdb, task, "done", "", "")
		w.log.Info().Uint("task_id", task.TaskID).Uint("woo_id", payload.WooID).
			Msg("woo worker: product set unavailable (manage_stock=false, stock_status=outofstock)")
		w.logImportBatchStatus(gdb, task.ImportID)
		return
	}

	// available: manage_stock=true, backorders=notify
	if product.ManageStock && product.Backorders == "notify" {
		if err := w.syncCacheFromVerifiedProduct(gdb, product, payload.TowarID); err != nil {
			w.failWooTask(gdb, task, fmt.Errorf("cache sync after already-set available: %w", err))
			return
		}
		w.completeWooTask(gdb, task, "done", "", "")
		w.log.Info().Uint("task_id", task.TaskID).Uint("woo_id", payload.WooID).
			Msg("woo worker: product already available and verified")
		w.logImportBatchStatus(gdb, task.ImportID)
		return
	}
	verified, err := w.updateAndVerifyProduct(ctx, payload.WooID, map[string]any{
		"manage_stock": true,
		"backorders":   "notify",
	})
	if err != nil {
		w.failWooTask(gdb, task, fmt.Errorf("update availability (available): %w", err))
		return
	}
	if !verified.ManageStock || verified.Backorders != "notify" {
		w.failWooTask(gdb, task, fmt.Errorf("availability verification mismatch: got manage_stock=%v backorders=%q", verified.ManageStock, verified.Backorders))
		return
	}
	if err := w.syncCacheFromVerifiedProduct(gdb, verified, payload.TowarID); err != nil {
		w.failWooTask(gdb, task, fmt.Errorf("cache sync after available update: %w", err))
		return
	}
	w.completeWooTask(gdb, task, "done", "", "")
	w.log.Info().Uint("task_id", task.TaskID).Uint("woo_id", payload.WooID).
		Msg("woo worker: product set available (manage_stock=true, backorders=notify)")
	w.logImportBatchStatus(gdb, task.ImportID)
}

func (w *Woo) fetchProduct(ctx context.Context, wooID uint) (wcProduct, error) {
	base, err := url.Parse(w.cfg.BaseURL)
	if err != nil {
		return wcProduct{}, err
	}
	base.Path = "/wp-json/wc/v3/products/" + strconv.FormatUint(uint64(wooID), 10)
	q := base.Query()
	q.Set("_fields", w.productFields())
	base.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return wcProduct{}, err
	}
	req.SetBasicAuth(w.cfg.ConsumerKey, w.cfg.ConsumerSec)
	req.Header.Set("User-Agent", "PCM2WWW/1.0")

	resp, err := w.client().Do(req)
	if err != nil {
		return wcProduct{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return wcProduct{}, fmt.Errorf("http %d", resp.StatusCode)
	}

	var product wcProduct
	if err := json.NewDecoder(resp.Body).Decode(&product); err != nil {
		return wcProduct{}, err
	}
	return product, nil
}

func (w *Woo) updateAndVerifyProduct(ctx context.Context, wooID uint, body map[string]any) (wcProduct, error) {
	base, err := url.Parse(w.cfg.BaseURL)
	if err != nil {
		return wcProduct{}, err
	}
	base.Path = "/wp-json/wc/v3/products/" + strconv.FormatUint(uint64(wooID), 10)

	rawBody, err := json.Marshal(body)
	if err != nil {
		return wcProduct{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, base.String(), bytes.NewReader(rawBody))
	if err != nil {
		return wcProduct{}, err
	}
	req.SetBasicAuth(w.cfg.ConsumerKey, w.cfg.ConsumerSec)
	req.Header.Set("User-Agent", "PCM2WWW/1.0")
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.client().Do(req)
	if err != nil {
		return wcProduct{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var payload map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&payload); err == nil {
			if raw, marshalErr := json.Marshal(payload); marshalErr == nil {
				return wcProduct{}, fmt.Errorf("http %d: %s", resp.StatusCode, string(raw))
			}
		}
		return wcProduct{}, fmt.Errorf("http %d", resp.StatusCode)
	}

	return w.fetchProduct(ctx, wooID)
}

func (w *Woo) syncCacheFromVerifiedProduct(gdb *gorm.DB, product wcProduct, towarID int64) error {
	row := db.WooProductCache{
		WooID:        uint(product.ID),
		TowarID:      ptrInt64(towarID),
		Kod:          product.SKU,
		Ean:          product.cacheEAN(),
		Name:         product.Name,
		PriceRegular: parsePrice(product.RegularPrice),
		PriceSale:    parsePrice(product.SalePrice),
		HurtPrice:    parsePrice(w.customFieldValue(product, "hurt_price")),
		TaxClass:     product.TaxClass,
		StockQty:     product.StockQuantity,
		StockManaged: product.ManageStock,
		StockStatus:  product.StockStatus,
		Backorders:   product.Backorders,
		Status:       product.Status,
		Type:         product.Type,
		DateModified: product.DateModifiedGMT,
	}

	return gdb.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "woo_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"towar_id", "kod", "ean", "name", "price_regular", "price_sale", "hurt_price", "tax_class",
			"stock_qty", "stock_managed", "stock_status", "backorders", "status", "type", "date_modified",
		}),
	}).Create(&row).Error
}

func (w *Woo) failWooTask(gdb *gorm.DB, task db.WooTask, err error) {
	if isWorkerContextInterruption(err) {
		w.requeueWooTask(gdb, task, err)
		return
	}

	msg := err.Error()
	now := time.Now()
	_ = gdb.Model(&db.WooTask{}).
		Where("task_id = ?", task.TaskID).
		Updates(map[string]any{
			"status":      "error",
			"last_error":  msg,
			"finished_at": now,
		}).Error
	w.log.Error().
		Err(err).
		Uint("task_id", task.TaskID).
		Uint("import_id", task.ImportID).
		Str("kind", task.Kind).
		Msg("woo worker: task failed")
	w.logImportBatchStatus(gdb, task.ImportID)
}

func (w *Woo) requeueWooTask(gdb *gorm.DB, task db.WooTask, err error) {
	_ = gdb.Model(&db.WooTask{}).
		Where("task_id = ?", task.TaskID).
		Updates(map[string]any{
			"status":      "pending",
			"last_error":  "",
			"started_at":  nil,
			"finished_at": nil,
		}).Error
	w.log.Warn().
		Err(err).
		Uint("task_id", task.TaskID).
		Uint("import_id", task.ImportID).
		Str("kind", task.Kind).
		Msg("woo worker: task interrupted, returned to pending")
	w.logImportBatchStatus(gdb, task.ImportID)
}

func (w *Woo) completeWooTask(gdb *gorm.DB, task db.WooTask, status, detail, responseEAN string) {
	now := time.Now()
	lastError := detail
	if status == "done" {
		lastError = ""
	}
	_ = gdb.Model(&db.WooTask{}).
		Where("task_id = ?", task.TaskID).
		Updates(map[string]any{
			"status":      status,
			"last_error":  lastError,
			"finished_at": now,
		}).Error
}

func (w *Woo) logImportBatchStatus(gdb *gorm.DB, importID uint) {
	if importID == 0 {
		return
	}

	var filename string
	_ = gdb.Model(&db.ImportFile{}).Where("import_id = ?", importID).Select("filename").Scan(&filename).Error

	var rows []struct {
		Status string
		Count  int
	}
	if err := gdb.Model(&db.WooTask{}).
		Select("status, COUNT(*) AS count").
		Where("import_id = ?", importID).
		Group("status").
		Find(&rows).Error; err != nil {
		w.log.Error().Err(err).Uint("import_id", importID).Msg("woo worker: batch status query failed")
		return
	}

	counts := make(map[string]int, len(rows))
	for _, row := range rows {
		counts[row.Status] = row.Count
	}

	w.log.Info().
		Uint("import_id", importID).
		Str("file", filename).
		Int("pending", counts["pending"]).
		Int("running", counts["running"]).
		Int("done", counts["done"]).
		Int("skipped", counts["skipped"]).
		Int("error", counts["error"]).
		Msg("woo worker: import batch task status")
}

func (w *Woo) client() *http.Client {
	if w.http != nil {
		return w.http
	}
	return &http.Client{Timeout: 20 * time.Second}
}

func formatWooPrice(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func floatAlmostEqual(a, b float64) bool {
	return math.Abs(a-b) < 0.0001
}

func isWorkerContextInterruption(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func ptrInt64(v int64) *int64 {
	return &v
}

// executeBatch przekazuje grupę tasków do właściwego batch handlera.
func (w *Woo) executeBatch(ctx context.Context, gdb *gorm.DB, kind string, tasks []db.WooTask) {
	switch kind {
	case db.WooTaskKindPriceUpdate:
		w.handlePriceUpdateBatch(ctx, gdb, tasks)
	case db.WooTaskKindStockUpdate:
		w.handleStockUpdateBatch(ctx, gdb, tasks)
	}
}

func (w *Woo) handlePriceUpdateBatch(ctx context.Context, gdb *gorm.DB, tasks []db.WooTask) {
	// 1. Parsuj payloady
	type entry struct {
		task    db.WooTask
		payload db.WooPriceUpdatePayload
	}
	entries := make([]entry, 0, len(tasks))
	for _, task := range tasks {
		var p db.WooPriceUpdatePayload
		if err := json.Unmarshal([]byte(task.PayloadJSON), &p); err != nil {
			w.failWooTask(gdb, task, fmt.Errorf("unmarshal payload: %w", err))
			continue
		}
		entries = append(entries, entry{task, p})
	}
	if len(entries) == 0 {
		return
	}

	// 2. Batch GET
	wooIDs := make([]uint, len(entries))
	for i, e := range entries {
		wooIDs[i] = e.payload.WooID
	}
	live, err := w.fetchProductsBatch(ctx, wooIDs)
	if err != nil {
		for _, e := range entries {
			w.failWooTask(gdb, e.task, fmt.Errorf("batch GET: %w", err))
		}
		return
	}

	// 3. Policy check + buduj listę do aktualizacji
	type pending struct {
		entry  entry
		update map[string]any
	}
	var toUpdate []pending
	byWooID := make(map[uint]entry, len(entries))

	for _, e := range entries {
		product, ok := live[e.payload.WooID]
		if !ok {
			w.failWooTask(gdb, e.task, fmt.Errorf("product %d missing in batch GET response", e.payload.WooID))
			continue
		}
		switch {
		case parsePrice(product.SalePrice) > 0:
			_ = w.syncCacheFromVerifiedProduct(gdb, product, e.payload.TowarID)
			w.completeWooTask(gdb, e.task, "skipped", fmt.Sprintf("policy skip: live sale_price=%v", product.SalePrice), "")
		case floatAlmostEqual(parsePrice(product.RegularPrice), e.payload.DesiredRegular) &&
			floatAlmostEqual(parsePrice(w.customFieldValue(product, "hurt_price")), e.payload.DesiredHurt) &&
			product.TaxClass == e.payload.DesiredTaxClass:
			_ = w.syncCacheFromVerifiedProduct(gdb, product, e.payload.TowarID)
			w.completeWooTask(gdb, e.task, "done", "", "")
		default:
			upd := map[string]any{
				"id":            e.payload.WooID,
				"regular_price": formatWooPrice(e.payload.DesiredRegular),
				"tax_class":     e.payload.DesiredTaxClass,
			}
			w.applyCustomFieldPayload(upd, "hurt_price", formatWooPrice(e.payload.DesiredHurt))
			toUpdate = append(toUpdate, pending{e, upd})
			byWooID[e.payload.WooID] = e
		}
	}

	if len(toUpdate) == 0 {
		return
	}

	// 4. Batch POST
	updates := make([]map[string]any, len(toUpdate))
	for i, p := range toUpdate {
		updates[i] = p.update
	}
	verified, err := w.batchUpdateProducts(ctx, updates)
	if err != nil {
		for _, p := range toUpdate {
			w.failWooTask(gdb, p.entry.task, fmt.Errorf("batch POST: %w", err))
		}
		return
	}

	// 5. Weryfikacja i sync cache
	verifiedIDs := make(map[uint]struct{}, len(verified))
	for _, prod := range verified {
		e, ok := byWooID[uint(prod.ID)]
		if !ok {
			continue
		}
		verifiedIDs[uint(prod.ID)] = struct{}{}
		if !floatAlmostEqual(parsePrice(prod.RegularPrice), e.payload.DesiredRegular) ||
			!floatAlmostEqual(parsePrice(w.customFieldValue(prod, "hurt_price")), e.payload.DesiredHurt) ||
			prod.TaxClass != e.payload.DesiredTaxClass {
			w.failWooTask(gdb, e.task, fmt.Errorf(
				"price verification mismatch: got regular=%v hurt=%v tax=%v want regular=%v hurt=%v tax=%v",
				parsePrice(prod.RegularPrice), parsePrice(w.customFieldValue(prod, "hurt_price")), prod.TaxClass,
				e.payload.DesiredRegular, e.payload.DesiredHurt, e.payload.DesiredTaxClass,
			))
			continue
		}
		_ = w.syncCacheFromVerifiedProduct(gdb, prod, e.payload.TowarID)
		w.completeWooTask(gdb, e.task, "done", "", "")
	}
	// Taski których Woo nie zwróciło w odpowiedzi → fail
	for _, p := range toUpdate {
		if _, ok := verifiedIDs[p.entry.payload.WooID]; !ok {
			w.failWooTask(gdb, p.entry.task, fmt.Errorf("product %d missing in batch POST response", p.entry.payload.WooID))
		}
	}
	w.logImportBatchStatus(gdb, tasks[0].ImportID)
}

func (w *Woo) handleStockUpdateBatch(ctx context.Context, gdb *gorm.DB, tasks []db.WooTask) {
	// 1. Parsuj payloady
	type entry struct {
		task    db.WooTask
		payload db.WooStockUpdatePayload
	}
	entries := make([]entry, 0, len(tasks))
	for _, task := range tasks {
		var p db.WooStockUpdatePayload
		if err := json.Unmarshal([]byte(task.PayloadJSON), &p); err != nil {
			w.failWooTask(gdb, task, fmt.Errorf("unmarshal payload: %w", err))
			continue
		}
		entries = append(entries, entry{task, p})
	}
	if len(entries) == 0 {
		return
	}

	// 2. Batch GET
	wooIDs := make([]uint, len(entries))
	for i, e := range entries {
		wooIDs[i] = e.payload.WooID
	}
	live, err := w.fetchProductsBatch(ctx, wooIDs)
	if err != nil {
		for _, e := range entries {
			w.failWooTask(gdb, e.task, fmt.Errorf("batch GET: %w", err))
		}
		return
	}

	// 3. Policy check + buduj listę do aktualizacji
	type pending struct {
		entry  entry
		update map[string]any
	}
	var toUpdate []pending
	byWooID := make(map[uint]entry, len(entries))

	for _, e := range entries {
		product, ok := live[e.payload.WooID]
		if !ok {
			w.failWooTask(gdb, e.task, fmt.Errorf("product %d missing in batch GET response", e.payload.WooID))
			continue
		}
		switch {
		case !product.ManageStock:
			_ = w.syncCacheFromVerifiedProduct(gdb, product, e.payload.TowarID)
			w.completeWooTask(gdb, e.task, "skipped", "policy skip: live product has manage_stock=false", "")
		case floatAlmostEqual(product.StockQuantity, e.payload.DesiredStock):
			_ = w.syncCacheFromVerifiedProduct(gdb, product, e.payload.TowarID)
			w.completeWooTask(gdb, e.task, "done", "", "")
		default:
			toUpdate = append(toUpdate, pending{e, map[string]any{
				"id":             e.payload.WooID,
				"stock_quantity": e.payload.DesiredStock,
			}})
			byWooID[e.payload.WooID] = e
		}
	}

	if len(toUpdate) == 0 {
		return
	}

	// 4. Batch POST
	updates := make([]map[string]any, len(toUpdate))
	for i, p := range toUpdate {
		updates[i] = p.update
	}
	verified, err := w.batchUpdateProducts(ctx, updates)
	if err != nil {
		for _, p := range toUpdate {
			w.failWooTask(gdb, p.entry.task, fmt.Errorf("batch POST: %w", err))
		}
		return
	}

	// 5. Weryfikacja i sync cache
	verifiedIDs := make(map[uint]struct{}, len(verified))
	for _, prod := range verified {
		e, ok := byWooID[uint(prod.ID)]
		if !ok {
			continue
		}
		verifiedIDs[uint(prod.ID)] = struct{}{}
		if !floatAlmostEqual(prod.StockQuantity, e.payload.DesiredStock) {
			w.failWooTask(gdb, e.task, fmt.Errorf("stock verification mismatch: got %v want %v", prod.StockQuantity, e.payload.DesiredStock))
			continue
		}
		_ = w.syncCacheFromVerifiedProduct(gdb, prod, e.payload.TowarID)
		w.completeWooTask(gdb, e.task, "done", "", "")
	}
	for _, p := range toUpdate {
		if _, ok := verifiedIDs[p.entry.payload.WooID]; !ok {
			w.failWooTask(gdb, p.entry.task, fmt.Errorf("product %d missing in batch POST response", p.entry.payload.WooID))
		}
	}
	w.logImportBatchStatus(gdb, tasks[0].ImportID)
}

// fetchProductsBatch pobiera wiele produktów jednym GET (?include=id1,id2,...).
func (w *Woo) fetchProductsBatch(ctx context.Context, wooIDs []uint) (map[uint]wcProduct, error) {
	if len(wooIDs) == 0 {
		return nil, nil
	}
	base, err := url.Parse(w.cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	base.Path = "/wp-json/wc/v3/products"
	ids := make([]string, len(wooIDs))
	for i, id := range wooIDs {
		ids[i] = strconv.FormatUint(uint64(id), 10)
	}
	q := base.Query()
	q.Set("include", strings.Join(ids, ","))
	q.Set("per_page", strconv.Itoa(len(wooIDs)))
	q.Set("_fields", w.productFields())
	base.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(w.cfg.ConsumerKey, w.cfg.ConsumerSec)
	req.Header.Set("User-Agent", "PCM2WWW/1.0")

	resp, err := w.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("batch GET http %d", resp.StatusCode)
	}

	var products []wcProduct
	if err := json.NewDecoder(resp.Body).Decode(&products); err != nil {
		return nil, err
	}
	result := make(map[uint]wcProduct, len(products))
	for _, p := range products {
		result[uint(p.ID)] = p
	}
	return result, nil
}

type wcBatchResponse struct {
	Update []wcProduct `json:"update"`
}

// batchUpdateProducts wysyła POST /products/batch {"update": [...]} i zwraca zaktualizowane produkty.
func (w *Woo) batchUpdateProducts(ctx context.Context, updates []map[string]any) ([]wcProduct, error) {
	if len(updates) == 0 {
		return nil, nil
	}
	base, err := url.Parse(w.cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	base.Path = "/wp-json/wc/v3/products/batch"

	rawBody, err := json.Marshal(map[string]any{"update": updates})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base.String(), bytes.NewReader(rawBody))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(w.cfg.ConsumerKey, w.cfg.ConsumerSec)
	req.Header.Set("User-Agent", "PCM2WWW/1.0")
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var payload map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&payload); err == nil {
			if raw, merr := json.Marshal(payload); merr == nil {
				return nil, fmt.Errorf("batch POST http %d: %s", resp.StatusCode, string(raw))
			}
		}
		return nil, fmt.Errorf("batch POST http %d", resp.StatusCode)
	}

	var batchResp wcBatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&batchResp); err != nil {
		return nil, err
	}
	return batchResp.Update, nil
}
