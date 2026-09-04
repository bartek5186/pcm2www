package woocommerce

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bartek5186/pcm2www/internal/db"
	"github.com/bartek5186/pcm2www/internal/integrations"
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
var batchableKinds = []string{db.WooTaskKindPriceUpdate, db.WooTaskKindStockUpdate, db.WooTaskKindAvailabilityUpdate}

const workerBatchSize = 20

const maxWooTaskAttempts = 5

type wooHTTPError struct {
	StatusCode int
	Body       string
	RetryAfter time.Duration
}

func (e *wooHTTPError) Error() string {
	if strings.TrimSpace(e.Body) == "" {
		return fmt.Sprintf("http %d", e.StatusCode)
	}
	return fmt.Sprintf("http %d: %s", e.StatusCode, e.Body)
}

func recoverRunningWooTasks(gdb *gorm.DB) (int64, error) {
	res := gdb.Model(&db.WooTask{}).
		Where("status = ?", "running").
		Updates(map[string]any{
			"status":          "pending",
			"last_error":      "recovered after interrupted worker",
			"next_attempt_at": nil,
			"started_at":      nil,
			"finished_at":     nil,
		})
	return res.RowsAffected, res.Error
}

func (w *Woo) lockWooProducts(wooIDs []uint) func() {
	stripes := make([]int, 0, len(wooIDs))
	seen := make(map[int]struct{}, len(wooIDs))
	for _, wooID := range wooIDs {
		stripe := int(wooID % uint(len(w.productLocks)))
		if _, ok := seen[stripe]; ok {
			continue
		}
		seen[stripe] = struct{}{}
		stripes = append(stripes, stripe)
	}
	sort.Ints(stripes)
	for _, stripe := range stripes {
		w.productLocks[stripe].Lock()
	}
	return func() {
		for idx := len(stripes) - 1; idx >= 0; idx-- {
			w.productLocks[stripes[idx]].Unlock()
		}
	}
}

func (w *Woo) completeIfObsolete(gdb *gorm.DB, task db.WooTask) bool {
	if task.WooID == nil {
		return false
	}
	var newer int64
	if err := gdb.Model(&db.WooTask{}).
		Where("woo_id = ? AND kind = ? AND task_id > ? AND status <> ?", *task.WooID, task.Kind, task.TaskID, "superseded").
		Count(&newer).Error; err != nil {
		w.failWooTask(gdb, task, fmt.Errorf("check for newer task: %w", err))
		return true
	}
	if newer == 0 {
		return false
	}
	w.completeWooTask(gdb, task, "superseded", "newer task exists for this product and operation", "")
	return true
}

func (w *Woo) completeIfLinkStale(gdb *gorm.DB, task db.WooTask) bool {
	if task.Kind == db.WooTaskKindEANUpdate {
		return false
	}
	current, reason, err := wooTaskLinkCurrent(gdb, task)
	if err != nil {
		w.failWooTask(gdb, task, fmt.Errorf("validate current EAN link: %w", err))
		return true
	}
	if current {
		return false
	}
	w.completeWooTask(gdb, task, "superseded", reason, "")
	w.log.Warn().Uint("task_id", task.TaskID).Str("reason", reason).Msg("woo worker: stale task superseded")
	return true
}

func wooTaskLinkCurrent(gdb *gorm.DB, task db.WooTask) (bool, string, error) {
	if task.WooID == nil || task.TowarID == nil {
		return false, "task has no Woo/source identity", nil
	}
	var cache db.WooProductCache
	if err := gdb.Where("woo_id = ?", *task.WooID).Take(&cache).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, "Woo product is no longer present in cache", nil
		}
		return false, "", err
	}
	if cache.TowarID == nil || *cache.TowarID != *task.TowarID {
		return false, "Woo product is no longer linked to this source product", nil
	}
	var products []db.StProduct
	if err := gdb.Where("towar_id = ?", *task.TowarID).Limit(2).Find(&products).Error; err != nil {
		return false, "", err
	}
	if len(products) != 1 {
		return false, "source product is missing or ambiguous", nil
	}
	sourceEAN := integrations.NormalizeEAN(products[0].Kod)
	cacheEAN := integrations.NormalizeEAN(cache.Ean)
	if sourceEAN == "" || cacheEAN == "" || sourceEAN != cacheEAN {
		return false, "EAN link changed after task was planned", nil
	}
	return true, "", nil
}

func (w *Woo) completeIfLiveEANStale(gdb *gorm.DB, task db.WooTask, product wcProduct) bool {
	if task.TowarID == nil {
		w.completeWooTask(gdb, task, "superseded", "task has no source identity", "")
		return true
	}
	var source db.StProduct
	if err := gdb.Where("towar_id = ?", *task.TowarID).Take(&source).Error; err != nil {
		w.failWooTask(gdb, task, fmt.Errorf("revalidate source EAN against live Woo product: %w", err))
		return true
	}
	sourceEAN := integrations.NormalizeEAN(source.Kod)
	liveEAN := integrations.NormalizeEAN(product.cacheEAN())
	if sourceEAN != "" && sourceEAN == liveEAN {
		return false
	}
	if err := gdb.Model(&db.WooProductCache{}).Where("woo_id = ?", uint(product.ID)).Updates(map[string]any{
		"ean": product.cacheEAN(), "towar_id": nil,
	}).Error; err != nil {
		w.failWooTask(gdb, task, fmt.Errorf("clear cache link after live EAN change: %w", err))
		return true
	}
	w.completeWooTask(gdb, task, "superseded", "live Woo EAN no longer matches source EAN", "")
	return true
}

func (w *Woo) workerTick(ctx context.Context, gdb *gorm.DB) {
	for {
		if ctx.Err() != nil {
			return
		}
		if delay := w.circuitDelay(); delay > 0 {
			w.log.Warn().Dur("retry_in", delay).Msg("woo worker: circuit breaker open")
			return
		}

		// 1) Spróbuj batch dla każdego batchable kind
		didBatch := false
		for _, kind := range batchableKinds {
			claimCtx, cancelClaim := context.WithTimeout(ctx, 10*time.Second)
			tasks, err := claimNextNWooTasksOfKind(gdb.WithContext(claimCtx), kind, workerBatchSize)
			cancelClaim()
			if err != nil {
				w.log.Error().Err(err).Str("kind", kind).Msg("woo worker: claim batch failed")
				w.recordFatal(err)
				return
			}
			if len(tasks) == 0 {
				continue
			}
			dbCtx, cancelDB := context.WithTimeout(context.Background(), 30*time.Second)
			w.executeBatch(ctx, gdb.WithContext(dbCtx), kind, tasks)
			cancelDB()
			didBatch = true
			break
		}
		if didBatch {
			continue
		}

		// 2) Pozostałe (w tym stare ean.update) — sekwencyjnie
		claimCtx, cancelClaim := context.WithTimeout(ctx, 10*time.Second)
		task, err := claimNextSequentialWooTask(gdb.WithContext(claimCtx))
		cancelClaim()
		if err != nil {
			w.log.Error().Err(err).Msg("woo worker: claim task failed")
			w.recordFatal(err)
			return
		}
		if task == nil {
			return
		}
		dbCtx, cancelDB := context.WithTimeout(context.Background(), 30*time.Second)
		w.executeWooTask(ctx, gdb.WithContext(dbCtx), *task)
		cancelDB()
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
	for range 5 {
		var tasks []db.WooTask
		if err := gdb.
			Where("status = ? AND kind = ? AND (next_attempt_at IS NULL OR next_attempt_at <= ?)", "pending", kind, time.Now()).
			Order("created_at ASC, task_id ASC").
			Limit(1).
			Find(&tasks).Error; err != nil {
			return nil, err
		}
		if len(tasks) == 0 {
			return nil, nil // naprawdę brak tasków
		}
		task, err := claimWooTask(gdb, tasks[0])
		if err != nil {
			return nil, err
		}
		if task != nil {
			return task, nil // sukces
		}
		// RowsAffected=0 — inny worker przejął ten task, retry SELECT
	}
	return nil, nil
}

// claimNextSequentialWooTask claim-uje jeden task spoza batchableKinds.
func claimNextSequentialWooTask(gdb *gorm.DB) (*db.WooTask, error) {
	var tasks []db.WooTask
	if err := gdb.
		Where("status = ? AND kind NOT IN ? AND (next_attempt_at IS NULL OR next_attempt_at <= ?)", "pending", batchableKinds, time.Now()).
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
			"status":          "running",
			"started_at":      now,
			"attempts":        gorm.Expr("attempts + 1"),
			"last_error":      "",
			"next_attempt_at": nil,
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
	if task.WooID != nil {
		unlock := w.lockWooProducts([]uint{*task.WooID})
		defer unlock()
		if w.completeIfObsolete(gdb, task) {
			return
		}
		if w.completeIfLinkStale(gdb, task) {
			return
		}
	}

	w.log.Info().
		Uint("task_id", task.TaskID).
		Uint("import_id", task.ImportID).
		Str("kind", task.Kind).
		Interface("woo_id", task.WooID).
		Interface("towar_id", task.TowarID).
		Msg("woo worker: processing task")

	switch task.Kind {
	case db.WooTaskKindEANUpdate:
		// EAN jest wyłącznie kluczem linkowania PCM ↔ Woo. Nie zmieniamy go
		// automatycznie; ten przypadek bezpiecznie wygasza stare zadania w bazie.
		w.completeWooTask(gdb, task, "skipped", "EAN updates disabled: EAN is the link key", "")
		w.log.Warn().Uint("task_id", task.TaskID).Uint("import_id", task.ImportID).
			Msg("woo worker: skipped legacy EAN update task")
		w.logImportBatchStatus(gdb, task.ImportID)

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

func (w *Woo) handleStockUpdate(ctx context.Context, gdb *gorm.DB, task db.WooTask, payload db.WooStockUpdatePayload) {
	product, err := w.fetchProduct(ctx, payload.WooID)
	if err != nil {
		w.failWooTask(gdb, task, fmt.Errorf("fetch live product before stock update: %w", err))
		return
	}
	if w.completeIfLiveEANStale(gdb, task, product) {
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
	if w.completeIfLiveEANStale(gdb, task, product) {
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

func availabilityMatches(product wcProduct, payload db.WooAvailabilityPayload) bool {
	if payload.Unavailable {
		return !product.ManageStock && product.StockStatus == "outofstock" && product.CatalogVisibility == "hidden"
	}
	if !product.ManageStock || product.Backorders != "notify" || product.CatalogVisibility == "hidden" {
		return false
	}
	return !payload.SetStock || floatAlmostEqual(product.StockQuantity, payload.DesiredStock)
}

func availabilityUpdateBody(payload db.WooAvailabilityPayload) map[string]any {
	if payload.Unavailable {
		return map[string]any{
			"manage_stock":       false,
			"stock_status":       "outofstock",
			"catalog_visibility": "hidden",
		}
	}
	body := map[string]any{
		"manage_stock":       true,
		"backorders":         "notify",
		"catalog_visibility": "visible",
	}
	if payload.SetStock {
		body["stock_quantity"] = payload.DesiredStock
	}
	return body
}

func (w *Woo) handleAvailabilityUpdate(ctx context.Context, gdb *gorm.DB, task db.WooTask, payload db.WooAvailabilityPayload) {
	product, err := w.fetchProduct(ctx, payload.WooID)
	if err != nil {
		w.failWooTask(gdb, task, fmt.Errorf("fetch live product before availability update: %w", err))
		return
	}
	if w.completeIfLiveEANStale(gdb, task, product) {
		return
	}

	if payload.Unavailable {
		if availabilityMatches(product, payload) {
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
		verified, err := w.updateAndVerifyProduct(ctx, payload.WooID, availabilityUpdateBody(payload))
		if err != nil {
			w.failWooTask(gdb, task, fmt.Errorf("update availability (unavailable): %w", err))
			return
		}
		if !availabilityMatches(verified, payload) {
			w.failWooTask(gdb, task, fmt.Errorf("availability verification mismatch: got manage_stock=%v stock_status=%q catalog_visibility=%q", verified.ManageStock, verified.StockStatus, verified.CatalogVisibility))
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

	// available: manage_stock=true, backorders=notify, catalog_visibility=visible
	if availabilityMatches(product, payload) {
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
	verified, err := w.updateAndVerifyProduct(ctx, payload.WooID, availabilityUpdateBody(payload))
	if err != nil {
		w.failWooTask(gdb, task, fmt.Errorf("update availability (available): %w", err))
		return
	}
	if !availabilityMatches(verified, payload) {
		w.failWooTask(gdb, task, fmt.Errorf("availability verification mismatch: got manage_stock=%v backorders=%q stock=%v", verified.ManageStock, verified.Backorders, verified.StockQuantity))
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
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
		return wcProduct{}, wooErrorFromResponse(resp, raw)
	}

	var product wcProduct
	if err := json.NewDecoder(resp.Body).Decode(&product); err != nil {
		return wcProduct{}, err
	}
	w.recordWooSuccess()
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
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
		return wcProduct{}, wooErrorFromResponse(resp, raw)
	}

	return w.fetchProduct(ctx, wooID)
}

func (w *Woo) syncCacheFromVerifiedProduct(gdb *gorm.DB, product wcProduct, towarID int64) error {
	row := db.WooProductCache{
		WooID:             uint(product.ID),
		TowarID:           ptrInt64(towarID),
		Kod:               product.SKU,
		Ean:               product.cacheEAN(),
		Name:              product.Name,
		PriceRegular:      parsePrice(product.RegularPrice),
		PriceSale:         parsePrice(product.SalePrice),
		HurtPrice:         parsePrice(w.customFieldValue(product, "hurt_price")),
		TaxClass:          product.TaxClass,
		StockQty:          product.StockQuantity,
		StockManaged:      product.ManageStock,
		StockStatus:       product.StockStatus,
		Backorders:        product.Backorders,
		CatalogVisibility: product.CatalogVisibility,
		Status:            product.Status,
		Type:              product.Type,
		DateModified:      product.DateModifiedGMT,
	}

	return gdb.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "woo_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"towar_id", "kod", "ean", "name", "price_regular", "price_sale", "hurt_price", "tax_class",
			"stock_qty", "stock_managed", "stock_status", "backorders", "catalog_visibility", "status", "type", "date_modified",
		}),
	}).Create(&row).Error
}

func (w *Woo) failWooTask(gdb *gorm.DB, task db.WooTask, err error) {
	if errors.Is(err, context.Canceled) {
		w.requeueWooTask(gdb, task, err)
		return
	}
	if isTransientWooError(err) && task.Attempts < maxWooTaskAttempts {
		w.retryWooTask(gdb, task, err)
		return
	}

	msg := err.Error()
	now := time.Now()
	if updateErr := gdb.Model(&db.WooTask{}).
		Where("task_id = ?", task.TaskID).
		Updates(map[string]any{
			"status":          "error",
			"last_error":      msg,
			"next_attempt_at": nil,
			"finished_at":     now,
		}).Error; updateErr != nil {
		w.recordFatal(fmt.Errorf("persist failed task %d: %w", task.TaskID, updateErr))
	}
	w.log.Error().
		Err(err).
		Uint("task_id", task.TaskID).
		Uint("import_id", task.ImportID).
		Str("kind", task.Kind).
		Msg("woo worker: task failed")
	w.logImportBatchStatus(gdb, task.ImportID)
}

func (w *Woo) retryWooTask(gdb *gorm.DB, task db.WooTask, err error) {
	w.recordTransientWooFailure()
	nextAttempt := time.Now().Add(wooRetryDelayForError(task, err))
	if updateErr := gdb.Model(&db.WooTask{}).
		Where("task_id = ?", task.TaskID).
		Updates(map[string]any{
			"status":          "pending",
			"last_error":      err.Error(),
			"next_attempt_at": nextAttempt,
			"started_at":      nil,
			"finished_at":     nil,
		}).Error; updateErr != nil {
		w.recordFatal(fmt.Errorf("persist retry task %d: %w", task.TaskID, updateErr))
	}
	w.log.Warn().
		Err(err).
		Uint("task_id", task.TaskID).
		Int("attempt", task.Attempts).
		Time("next_attempt_at", nextAttempt).
		Msg("woo worker: transient failure, retry scheduled")
}

func (w *Woo) requeueWooTask(gdb *gorm.DB, task db.WooTask, err error) {
	if updateErr := gdb.Model(&db.WooTask{}).
		Where("task_id = ?", task.TaskID).
		Updates(map[string]any{
			"status":          "pending",
			"last_error":      "",
			"next_attempt_at": nil,
			"started_at":      nil,
			"finished_at":     nil,
		}).Error; updateErr != nil {
		w.recordFatal(fmt.Errorf("persist requeued task %d: %w", task.TaskID, updateErr))
	}
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
	if updateErr := gdb.Model(&db.WooTask{}).
		Where("task_id = ?", task.TaskID).
		Updates(map[string]any{
			"status":          status,
			"last_error":      lastError,
			"next_attempt_at": nil,
			"finished_at":     now,
		}).Error; updateErr != nil {
		w.recordFatal(fmt.Errorf("persist completed task %d: %w", task.TaskID, updateErr))
	}
}

func (w *Woo) logImportBatchStatus(gdb *gorm.DB, importID uint) {
	if importID == 0 {
		return
	}

	var filename string
	if err := gdb.Model(&db.ImportFile{}).Where("import_id = ?", importID).Select("filename").Scan(&filename).Error; err != nil {
		w.log.Error().Err(err).Uint("import_id", importID).Msg("woo worker: import filename query failed")
	}

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

func isTransientWooError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var httpErr *wooHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode == http.StatusTooManyRequests || httpErr.StatusCode >= http.StatusInternalServerError
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func wooRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := 5 * time.Second
	for n := 1; n < attempt && delay < 5*time.Minute; n++ {
		delay *= 2
	}
	if delay > 5*time.Minute {
		return 5 * time.Minute
	}
	return delay
}

func wooRetryDelayForError(task db.WooTask, err error) time.Duration {
	delay := wooRetryDelay(task.Attempts)
	jitterPermille := (uint64(task.TaskID)*1103515245 + uint64(task.Attempts)*12345) % 201
	delay += time.Duration(int64(delay) * int64(jitterPermille) / 1000)
	var httpErr *wooHTTPError
	if errors.As(err, &httpErr) && httpErr.RetryAfter > delay {
		delay = httpErr.RetryAfter
	}
	return delay
}

func wooErrorFromResponse(resp *http.Response, body []byte) *wooHTTPError {
	return &wooHTTPError{
		StatusCode: resp.StatusCode,
		Body:       strings.TrimSpace(string(body)),
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
	}
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}

func (w *Woo) recordTransientWooFailure() {
	w.circuitMu.Lock()
	defer w.circuitMu.Unlock()
	w.circuitFails++
	if w.circuitFails >= 3 {
		w.circuitUntil = time.Now().Add(30 * time.Second)
	}
}

func (w *Woo) recordWooSuccess() {
	w.circuitMu.Lock()
	w.circuitFails = 0
	w.circuitUntil = time.Time{}
	w.circuitMu.Unlock()
}

func (w *Woo) circuitDelay() time.Duration {
	w.circuitMu.Lock()
	defer w.circuitMu.Unlock()
	remaining := time.Until(w.circuitUntil)
	if remaining <= 0 {
		return 0
	}
	return remaining
}

func ptrInt64(v int64) *int64 {
	return &v
}

// executeBatch przekazuje grupę tasków do właściwego batch handlera.
func (w *Woo) executeBatch(ctx context.Context, gdb *gorm.DB, kind string, tasks []db.WooTask) {
	wooIDs := make([]uint, 0, len(tasks))
	for _, task := range tasks {
		if task.WooID != nil {
			wooIDs = append(wooIDs, *task.WooID)
		}
	}
	unlock := w.lockWooProducts(wooIDs)
	defer unlock()

	active := tasks[:0]
	for _, task := range tasks {
		if !w.completeIfObsolete(gdb, task) && !w.completeIfLinkStale(gdb, task) {
			active = append(active, task)
		}
	}
	if len(active) == 0 {
		return
	}
	tasks = active

	switch kind {
	case db.WooTaskKindPriceUpdate:
		w.handlePriceUpdateBatch(ctx, gdb, tasks)
	case db.WooTaskKindStockUpdate:
		w.handleStockUpdateBatch(ctx, gdb, tasks)
	case db.WooTaskKindAvailabilityUpdate:
		w.handleAvailabilityUpdateBatch(ctx, gdb, tasks)
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
		if w.completeIfLiveEANStale(gdb, e.task, product) {
			continue
		}
		switch {
		case parsePrice(product.SalePrice) > 0:
			if err := w.syncCacheFromVerifiedProduct(gdb, product, e.payload.TowarID); err != nil {
				w.failWooTask(gdb, e.task, fmt.Errorf("cache sync after price policy skip: %w", err))
				continue
			}
			w.completeWooTask(gdb, e.task, "skipped", fmt.Sprintf("policy skip: live sale_price=%v", product.SalePrice), "")
		case floatAlmostEqual(parsePrice(product.RegularPrice), e.payload.DesiredRegular) &&
			floatAlmostEqual(parsePrice(w.customFieldValue(product, "hurt_price")), e.payload.DesiredHurt) &&
			product.TaxClass == e.payload.DesiredTaxClass:
			if err := w.syncCacheFromVerifiedProduct(gdb, product, e.payload.TowarID); err != nil {
				w.failWooTask(gdb, e.task, fmt.Errorf("cache sync after price verification: %w", err))
				continue
			}
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
	_, err = w.batchUpdateProducts(ctx, updates)
	if err != nil {
		for _, p := range toUpdate {
			w.failWooTask(gdb, p.entry.task, fmt.Errorf("batch POST: %w", err))
		}
		return
	}
	verifiedByID, err := w.fetchProductsBatch(ctx, wooIDsFromUpdates(updates))
	if err != nil {
		for _, p := range toUpdate {
			w.failWooTask(gdb, p.entry.task, fmt.Errorf("batch verification GET: %w", err))
		}
		return
	}
	verified := productMapValues(verifiedByID)

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
		if err := w.syncCacheFromVerifiedProduct(gdb, prod, e.payload.TowarID); err != nil {
			w.failWooTask(gdb, e.task, fmt.Errorf("cache sync after price update: %w", err))
			continue
		}
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
		if w.completeIfLiveEANStale(gdb, e.task, product) {
			continue
		}
		switch {
		case !product.ManageStock:
			if err := w.syncCacheFromVerifiedProduct(gdb, product, e.payload.TowarID); err != nil {
				w.failWooTask(gdb, e.task, fmt.Errorf("cache sync after stock policy skip: %w", err))
				continue
			}
			w.completeWooTask(gdb, e.task, "skipped", "policy skip: live product has manage_stock=false", "")
		case floatAlmostEqual(product.StockQuantity, e.payload.DesiredStock):
			if err := w.syncCacheFromVerifiedProduct(gdb, product, e.payload.TowarID); err != nil {
				w.failWooTask(gdb, e.task, fmt.Errorf("cache sync after stock verification: %w", err))
				continue
			}
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
	_, err = w.batchUpdateProducts(ctx, updates)
	if err != nil {
		for _, p := range toUpdate {
			w.failWooTask(gdb, p.entry.task, fmt.Errorf("batch POST: %w", err))
		}
		return
	}
	verifiedByID, err := w.fetchProductsBatch(ctx, wooIDsFromUpdates(updates))
	if err != nil {
		for _, p := range toUpdate {
			w.failWooTask(gdb, p.entry.task, fmt.Errorf("batch verification GET: %w", err))
		}
		return
	}
	verified := productMapValues(verifiedByID)

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
		if err := w.syncCacheFromVerifiedProduct(gdb, prod, e.payload.TowarID); err != nil {
			w.failWooTask(gdb, e.task, fmt.Errorf("cache sync after stock update: %w", err))
			continue
		}
		w.completeWooTask(gdb, e.task, "done", "", "")
	}
	for _, p := range toUpdate {
		if _, ok := verifiedIDs[p.entry.payload.WooID]; !ok {
			w.failWooTask(gdb, p.entry.task, fmt.Errorf("product %d missing in batch POST response", p.entry.payload.WooID))
		}
	}
	w.logImportBatchStatus(gdb, tasks[0].ImportID)
}

func (w *Woo) handleAvailabilityUpdateBatch(ctx context.Context, gdb *gorm.DB, tasks []db.WooTask) {
	type entry struct {
		task    db.WooTask
		payload db.WooAvailabilityPayload
	}
	entries := make([]entry, 0, len(tasks))
	for _, task := range tasks {
		var p db.WooAvailabilityPayload
		if err := json.Unmarshal([]byte(task.PayloadJSON), &p); err != nil {
			w.failWooTask(gdb, task, fmt.Errorf("unmarshal payload: %w", err))
			continue
		}
		entries = append(entries, entry{task, p})
	}
	if len(entries) == 0 {
		return
	}

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
		if w.completeIfLiveEANStale(gdb, e.task, product) {
			continue
		}
		if availabilityMatches(product, e.payload) {
			if err := w.syncCacheFromVerifiedProduct(gdb, product, e.payload.TowarID); err != nil {
				w.failWooTask(gdb, e.task, fmt.Errorf("cache sync after availability verification: %w", err))
				continue
			}
			w.completeWooTask(gdb, e.task, "done", "", "")
			continue
		}
		upd := availabilityUpdateBody(e.payload)
		upd["id"] = e.payload.WooID
		toUpdate = append(toUpdate, pending{e, upd})
		byWooID[e.payload.WooID] = e
	}

	if len(toUpdate) == 0 {
		return
	}

	updates := make([]map[string]any, len(toUpdate))
	for i, p := range toUpdate {
		updates[i] = p.update
	}
	_, err = w.batchUpdateProducts(ctx, updates)
	if err != nil {
		for _, p := range toUpdate {
			w.failWooTask(gdb, p.entry.task, fmt.Errorf("batch POST: %w", err))
		}
		return
	}
	verifiedByID, err := w.fetchProductsBatch(ctx, wooIDsFromUpdates(updates))
	if err != nil {
		for _, p := range toUpdate {
			w.failWooTask(gdb, p.entry.task, fmt.Errorf("batch verification GET: %w", err))
		}
		return
	}
	verified := productMapValues(verifiedByID)

	verifiedIDs := make(map[uint]struct{}, len(verified))
	for _, prod := range verified {
		e, ok := byWooID[uint(prod.ID)]
		if !ok {
			continue
		}
		verifiedIDs[uint(prod.ID)] = struct{}{}
		if !availabilityMatches(prod, e.payload) {
			w.failWooTask(gdb, e.task, fmt.Errorf("availability verification mismatch: manage_stock=%v stock_status=%q backorders=%q catalog_visibility=%q", prod.ManageStock, prod.StockStatus, prod.Backorders, prod.CatalogVisibility))
			continue
		}
		if err := w.syncCacheFromVerifiedProduct(gdb, prod, e.payload.TowarID); err != nil {
			w.failWooTask(gdb, e.task, fmt.Errorf("cache sync after availability update: %w", err))
			continue
		}
		w.completeWooTask(gdb, e.task, "done", "", "")
	}
	for _, p := range toUpdate {
		if _, ok := verifiedIDs[p.entry.payload.WooID]; !ok {
			w.failWooTask(gdb, p.entry.task, fmt.Errorf("product %d missing in batch POST response", p.entry.payload.WooID))
		}
	}
	w.logImportBatchStatus(gdb, tasks[0].ImportID)
}

func wooIDsFromUpdates(updates []map[string]any) []uint {
	ids := make([]uint, 0, len(updates))
	for _, update := range updates {
		switch id := update["id"].(type) {
		case uint:
			ids = append(ids, id)
		case int:
			if id > 0 {
				ids = append(ids, uint(id))
			}
		}
	}
	return ids
}

func productMapValues(products map[uint]wcProduct) []wcProduct {
	ids := make([]int, 0, len(products))
	for id := range products {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	result := make([]wcProduct, 0, len(ids))
	for _, id := range ids {
		result = append(result, products[uint(id)])
	}
	return result
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
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
		return nil, wooErrorFromResponse(resp, raw)
	}

	var products []wcProduct
	if err := json.NewDecoder(resp.Body).Decode(&products); err != nil {
		return nil, err
	}
	result := make(map[uint]wcProduct, len(products))
	for _, p := range products {
		result[uint(p.ID)] = p
	}
	w.recordWooSuccess()
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
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
		return nil, wooErrorFromResponse(resp, raw)
	}

	var batchResp wcBatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&batchResp); err != nil {
		return nil, err
	}
	w.recordWooSuccess()
	return batchResp.Update, nil
}
