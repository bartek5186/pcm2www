package importer

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bartek5186/pcm2www/internal/db"
	"gorm.io/gorm"
)

type plannerSourceRow struct {
	ImportID       uint
	TowarID        int64
	Kod            string
	Nazwa          string
	CenaDetal      float64
	CenaHurtowa    float64
	AktywnyWSI     bool
	DoUsuniecia    bool
	TotalStock     float64
	TotalReserved  float64
	TotalStockPrev *float64 // NULL jeśli brak historii dla choć jednego magazynu
}

type plannerCacheRow struct {
	WooID        uint
	TowarID      *int64
	Kod          string
	Ean          string
	Name         string
	PriceRegular float64
	PriceSale    float64
	HurtPrice    float64
	StockQty     float64
	StockManaged bool
	StockStatus  string
	Backorders   string
}

type plannerStats struct {
	ImportID                    uint
	Filename                    string
	ProductsSeen                int
	LinkedProducts              int
	UnlinkedProducts            int
	AmbiguousProducts           int
	EANTasksCreated             int
	EANTasksRequeued            int
	StockTasksCreated           int
	StockTasksRequeued          int
	PriceTasksCreated           int
	PriceTasksRequeued          int
	AvailabilityTasksCreated    int
	AvailabilityTasksRequeued   int
	ExistingPendingOrDone       int
	PolicySkipEANPresent        int
	PolicySkipDuplicateEAN      int
	PolicySkipStockUnmanaged    int
	PolicySkipPriceSale         int
}

func (i *Importer) PlanWooTasksForImports(importIDs []uint) error {
	if len(importIDs) == 0 {
		return nil
	}

	uniq := make([]uint, 0, len(importIDs))
	seen := make(map[uint]struct{}, len(importIDs))
	for _, importID := range importIDs {
		if importID == 0 {
			continue
		}
		if _, ok := seen[importID]; ok {
			continue
		}
		seen[importID] = struct{}{}
		uniq = append(uniq, importID)
	}
	slices.Sort(uniq)

	for _, importID := range uniq {
		if err := i.PlanWooTasks(importID); err != nil {
			return err
		}
	}
	return nil
}

func (i *Importer) PlanWooTasksForRecentImports(window time.Duration) error {
	if window <= 0 {
		window = 24 * time.Hour
	}

	since := time.Now().Add(-window)
	var ids []uint
	if err := i.db.Model(&db.ImportFile{}).
		Where("status = ? AND processed_at IS NOT NULL AND processed_at >= ?", 1, since).
		Order("processed_at ASC").
		Pluck("import_id", &ids).Error; err != nil {
		return err
	}
	return i.PlanWooTasksForImports(ids)
}

func (i *Importer) PlanWooTasks(importID uint) error {
	tx := i.db.Begin()
	defer tx.Rollback()

	stats, err := i.planWooTasksTx(tx, importID)
	if err != nil {
		return err
	}

	if err := tx.Commit().Error; err != nil {
		return err
	}

	i.log.Info().
		Uint("import_id", stats.ImportID).
		Str("file", stats.Filename).
		Int("products_seen", stats.ProductsSeen).
		Int("linked_products", stats.LinkedProducts).
		Int("unlinked_products", stats.UnlinkedProducts).
		Int("ambiguous_products", stats.AmbiguousProducts).
		Int("ean_tasks_created", stats.EANTasksCreated).
		Int("ean_tasks_requeued", stats.EANTasksRequeued).
		Int("stock_tasks_created", stats.StockTasksCreated).
		Int("stock_tasks_requeued", stats.StockTasksRequeued).
		Int("price_tasks_created", stats.PriceTasksCreated).
		Int("price_tasks_requeued", stats.PriceTasksRequeued).
		Int("existing_tasks", stats.ExistingPendingOrDone).
		Int("skip_ean_present", stats.PolicySkipEANPresent).
		Int("skip_duplicate_ean", stats.PolicySkipDuplicateEAN).
		Int("skip_stock_unmanaged", stats.PolicySkipStockUnmanaged).
		Int("skip_price_sale", stats.PolicySkipPriceSale).
		Int("availability_tasks_created", stats.AvailabilityTasksCreated).
		Int("availability_tasks_requeued", stats.AvailabilityTasksRequeued).
		Msg("woo task planning finished")

	return nil
}

func (i *Importer) planWooTasksTx(tx *gorm.DB, importID uint) (plannerStats, error) {
	stats := plannerStats{ImportID: importID}

	var importFile db.ImportFile
	if err := tx.Where("import_id = ?", importID).Take(&importFile).Error; err != nil {
		return stats, fmt.Errorf("plan tasks import %d: %w", importID, err)
	}
	stats.Filename = importFile.Filename

	sourceRows, err := loadPlannerSourceRows(tx, importID)
	if err != nil {
		return stats, err
	}
	stats.ProductsSeen = len(sourceRows)
	if len(sourceRows) == 0 {
		return stats, nil
	}

	towarIDs := make([]int64, 0, len(sourceRows))
	for _, row := range sourceRows {
		towarIDs = append(towarIDs, row.TowarID)
	}

	cacheRows, err := loadPlannerCacheRows(tx, towarIDs)
	if err != nil {
		return stats, err
	}

	cacheByTowarID := make(map[int64][]plannerCacheRow, len(cacheRows))
	for _, row := range cacheRows {
		if row.TowarID == nil {
			continue
		}
		cacheByTowarID[*row.TowarID] = append(cacheByTowarID[*row.TowarID], row)
	}

	eanOwners, err := loadCacheEANOwners(tx)
	if err != nil {
		return stats, err
	}

	for _, row := range sourceRows {
		candidates := cacheByTowarID[row.TowarID]
		switch len(candidates) {
		case 0:
			stats.UnlinkedProducts++
			i.log.Debug().
				Uint("import_id", importID).
				Int64("towar_id", row.TowarID).
				Str("kod", row.Kod).
				Msg("task planner: skip unlinked product")
			continue
		case 1:
			stats.LinkedProducts++
		default:
			stats.AmbiguousProducts++
			i.log.Warn().
				Uint("import_id", importID).
				Int64("towar_id", row.TowarID).
				Int("matches", len(candidates)).
				Msg("task planner: skip ambiguous Woo link")
			continue
		}

		cache := candidates[0]

		if created, requeued, existed, err := i.planEANUpdateTask(tx, importID, row, cache, eanOwners); err != nil {
			return stats, err
		} else {
			switch {
			case created:
				stats.EANTasksCreated++
			case requeued:
				stats.EANTasksRequeued++
			case existed:
				stats.ExistingPendingOrDone++
			default:
				if cleanEAN(row.Kod) != "" && cleanEAN(cache.Ean) != "" && cleanEAN(row.Kod) != cleanEAN(cache.Ean) {
					stats.PolicySkipEANPresent++
				}
				if ownerIDs := eanOwners[cleanEAN(row.Kod)]; cleanEAN(row.Kod) != "" && len(ownerIDs) > 0 {
					dupOwners := 0
					for _, ownerID := range ownerIDs {
						if ownerID != cache.WooID {
							dupOwners++
						}
					}
					if dupOwners > 0 && cleanEAN(cache.Ean) == "" {
						stats.PolicySkipDuplicateEAN++
					}
				}
			}
		}

		if created, requeued, existed, err := i.planAvailabilityUpdateTask(tx, importID, row, cache); err != nil {
			return stats, err
		} else {
			switch {
			case created:
				stats.AvailabilityTasksCreated++
			case requeued:
				stats.AvailabilityTasksRequeued++
			case existed:
				stats.ExistingPendingOrDone++
			}
		}

		if created, requeued, existed, skipped, err := i.planStockUpdateTask(tx, importID, row, cache); err != nil {
			return stats, err
		} else {
			switch {
			case created:
				stats.StockTasksCreated++
			case requeued:
				stats.StockTasksRequeued++
			case existed:
				stats.ExistingPendingOrDone++
			case skipped:
				stats.PolicySkipStockUnmanaged++
			}
		}

		if created, requeued, existed, skipped, err := i.planPriceUpdateTask(tx, importID, row, cache); err != nil {
			return stats, err
		} else {
			switch {
			case created:
				stats.PriceTasksCreated++
			case requeued:
				stats.PriceTasksRequeued++
			case existed:
				stats.ExistingPendingOrDone++
			case skipped:
				stats.PolicySkipPriceSale++
			}
		}
	}

	return stats, nil
}

func loadPlannerSourceRows(tx *gorm.DB, importID uint) ([]plannerSourceRow, error) {
	const q = `
SELECT
	p.import_id,
	p.towar_id,
	p.kod,
	p.nazwa,
	p.cena_detal,
	p.cena_hurtowa,
	p.aktywny_wsi,
	p.do_usuniecia,
	COALESCE(SUM(s.stan), 0) AS total_stock,
	COALESCE(SUM(s.rezerwacja), 0) AS total_reserved,
	CASE WHEN COUNT(*) = COUNT(s.stan_prev) THEN SUM(s.stan_prev) ELSE NULL END AS total_stock_prev
FROM st_products p
LEFT JOIN st_stocks s ON s.towar_id = p.towar_id
WHERE p.import_id = ?
GROUP BY
	p.import_id,
	p.towar_id,
	p.kod,
	p.nazwa,
	p.cena_detal,
	p.cena_hurtowa,
	p.aktywny_wsi,
	p.do_usuniecia
ORDER BY p.towar_id;
`
	var rows []plannerSourceRow
	return rows, tx.Raw(q, importID).Scan(&rows).Error
}

func loadPlannerCacheRows(tx *gorm.DB, towarIDs []int64) ([]plannerCacheRow, error) {
	var rows []plannerCacheRow
	if len(towarIDs) == 0 {
		return rows, nil
	}
	if err := tx.Model(&db.WooProductCache{}).
		Where("towar_id IN ?", towarIDs).
		Select("woo_id", "towar_id", "kod", "ean", "name", "price_regular", "price_sale", "hurt_price", "stock_qty", "stock_managed", "stock_status", "backorders").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func loadCacheEANOwners(tx *gorm.DB) (map[string][]uint, error) {
	var rows []struct {
		WooID uint
		Ean   string
	}
	if err := tx.Model(&db.WooProductCache{}).
		Select("woo_id", "ean").
		Find(&rows).Error; err != nil {
		return nil, err
	}

	owners := make(map[string][]uint, len(rows))
	for _, row := range rows {
		ean := cleanEAN(row.Ean)
		if ean == "" {
			continue
		}
		owners[ean] = append(owners[ean], row.WooID)
	}
	return owners, nil
}

func (i *Importer) planEANUpdateTask(tx *gorm.DB, importID uint, src plannerSourceRow, cache plannerCacheRow, eanOwners map[string][]uint) (created, requeued, existed bool, err error) {
	desiredEAN := cleanEAN(src.Kod)
	currentEAN := cleanEAN(cache.Ean)
	if desiredEAN == "" || desiredEAN == currentEAN {
		return false, false, false, nil
	}
	if currentEAN != "" {
		i.log.Debug().
			Uint("import_id", importID).
			Uint("woo_id", cache.WooID).
			Int64("towar_id", src.TowarID).
			Str("current_ean", currentEAN).
			Str("desired_ean", desiredEAN).
			Msg("task planner: skip EAN overwrite by policy")
		return false, false, false, nil
	}
	for _, ownerID := range eanOwners[desiredEAN] {
		if ownerID == cache.WooID {
			continue
		}
		i.log.Warn().
			Uint("import_id", importID).
			Uint("woo_id", cache.WooID).
			Int64("towar_id", src.TowarID).
			Str("desired_ean", desiredEAN).
			Uint("owner_woo_id", ownerID).
			Msg("task planner: skip duplicate EAN by policy")
		return false, false, false, nil
	}

	payload := db.WooEANUpdatePayload{
		ImportID:    importID,
		WooID:       cache.WooID,
		TowarID:     src.TowarID,
		SKU:         cache.Kod,
		ProductName: cache.Name,
		SourceKod:   src.Kod,
		CurrentEAN:  currentEAN,
		DesiredEAN:  desiredEAN,
	}
	task := db.WooTask{
		TaskKey:     buildTaskKey(db.WooTaskKindEANUpdate, cache.WooID, desiredEAN),
		ImportID:    importID,
		TowarID:     ptrInt64(src.TowarID),
		WooID:       ptrUint(cache.WooID),
		Kind:        db.WooTaskKindEANUpdate,
		PayloadJSON: mustJSON(payload),
		Status:      "pending",
	}
	return enqueueWooTask(tx, task)
}

func (i *Importer) planStockUpdateTask(tx *gorm.DB, importID uint, src plannerSourceRow, cache plannerCacheRow) (created, requeued, existed, skipped bool, err error) {
	if floatAlmostEqual(src.CenaDetal, 0) {
		return false, false, false, false, nil // produkt niedostępny (brak ceny) — stock obsługuje availability.update
	}
	desiredStock := math.Max(src.TotalStock-src.TotalReserved, 0)
	if floatAlmostEqual(cache.StockQty, desiredStock) {
		return false, false, false, false, nil
	}
	// Jeśli mamy historię PCM i efektywny stan się nie zmienił, nie nadpisuj Woo —
	// różnica w cache może wynikać ze sprzedaży w sklepie (której PCM jeszcze nie zna).
	if src.TotalStockPrev != nil {
		prevNet := math.Max(*src.TotalStockPrev-src.TotalReserved, 0)
		if floatAlmostEqual(desiredStock, prevNet) {
			i.log.Debug().
				Uint("import_id", importID).
				Uint("woo_id", cache.WooID).
				Int64("towar_id", src.TowarID).
				Float64("pcm_stock", src.TotalStock).
				Float64("pcm_stock_prev", *src.TotalStockPrev).
				Float64("cache_stock", cache.StockQty).
				Msg("task planner: skip stock update — PCM unchanged, cache diff likely from Woo sale")
			return false, false, false, false, nil
		}
	}
	if !cache.StockManaged {
		i.log.Debug().
			Uint("import_id", importID).
			Uint("woo_id", cache.WooID).
			Int64("towar_id", src.TowarID).
			Float64("current_stock", cache.StockQty).
			Float64("desired_stock", desiredStock).
			Msg("task planner: skip stock update because manage_stock is false")
		return false, false, false, true, nil
	}

	payload := db.WooStockUpdatePayload{
		ImportID:      importID,
		WooID:         cache.WooID,
		TowarID:       src.TowarID,
		SKU:           cache.Kod,
		ProductName:   cache.Name,
		CurrentStock:  cache.StockQty,
		DesiredStock:  desiredStock,
		StockManaged:  cache.StockManaged,
		SourceStock:   src.TotalStock,
		SourceReserve: src.TotalReserved,
	}
	task := db.WooTask{
		TaskKey:     buildTaskKey(db.WooTaskKindStockUpdate, cache.WooID, normalizeFloatKey(desiredStock)),
		ImportID:    importID,
		TowarID:     ptrInt64(src.TowarID),
		WooID:       ptrUint(cache.WooID),
		Kind:        db.WooTaskKindStockUpdate,
		PayloadJSON: mustJSON(payload),
		Status:      "pending",
	}
	created, requeued, existed, err = enqueueWooTask(tx, task)
	return created, requeued, existed, false, err
}

func (i *Importer) planPriceUpdateTask(tx *gorm.DB, importID uint, src plannerSourceRow, cache plannerCacheRow) (created, requeued, existed, skipped bool, err error) {
	if floatAlmostEqual(src.CenaDetal, 0) {
		return false, false, false, false, nil // produkt niedostępny (brak ceny) — nie ustawiaj ceny 0
	}
	desiredRegular := src.CenaDetal
	desiredHurt := src.CenaHurtowa
	if floatAlmostEqual(cache.PriceRegular, desiredRegular) && floatAlmostEqual(cache.HurtPrice, desiredHurt) {
		return false, false, false, false, nil
	}
	if cache.PriceSale > 0 {
		i.log.Debug().
			Uint("import_id", importID).
			Uint("woo_id", cache.WooID).
			Int64("towar_id", src.TowarID).
			Float64("current_sale", cache.PriceSale).
			Float64("desired_regular", desiredRegular).
			Msg("task planner: skip price update because sale price is active")
		return false, false, false, true, nil
	}

	payload := db.WooPriceUpdatePayload{
		ImportID:       importID,
		WooID:          cache.WooID,
		TowarID:        src.TowarID,
		SKU:            cache.Kod,
		ProductName:    cache.Name,
		CurrentRegular: cache.PriceRegular,
		DesiredRegular: desiredRegular,
		CurrentSale:    cache.PriceSale,
		CurrentHurt:    cache.HurtPrice,
		DesiredHurt:    desiredHurt,
	}
	task := db.WooTask{
		TaskKey:     buildTaskKey(db.WooTaskKindPriceUpdate, cache.WooID, normalizeFloatKey(desiredRegular), normalizeFloatKey(desiredHurt)),
		ImportID:    importID,
		TowarID:     ptrInt64(src.TowarID),
		WooID:       ptrUint(cache.WooID),
		Kind:        db.WooTaskKindPriceUpdate,
		PayloadJSON: mustJSON(payload),
		Status:      "pending",
	}
	created, requeued, existed, err = enqueueWooTask(tx, task)
	return created, requeued, existed, false, err
}

func (i *Importer) planAvailabilityUpdateTask(tx *gorm.DB, importID uint, src plannerSourceRow, cache plannerCacheRow) (created, requeued, existed bool, err error) {
	unavailable := floatAlmostEqual(src.CenaDetal, 0)

	if unavailable {
		if !cache.StockManaged && cache.StockStatus == "outofstock" {
			return false, false, false, nil
		}
	} else {
		if cache.StockManaged && cache.Backorders == "notify" {
			return false, false, false, nil
		}
	}

	stateKey := "available"
	if unavailable {
		stateKey = "unavailable"
	}

	payload := db.WooAvailabilityPayload{
		ImportID:    importID,
		WooID:       cache.WooID,
		TowarID:     src.TowarID,
		SKU:         cache.Kod,
		ProductName: cache.Name,
		Unavailable: unavailable,
	}
	task := db.WooTask{
		TaskKey:     buildTaskKey(db.WooTaskKindAvailabilityUpdate, cache.WooID, stateKey),
		ImportID:    importID,
		TowarID:     ptrInt64(src.TowarID),
		WooID:       ptrUint(cache.WooID),
		Kind:        db.WooTaskKindAvailabilityUpdate,
		PayloadJSON: mustJSON(payload),
		Status:      "pending",
	}
	return enqueueWooTask(tx, task)
}

func enqueueWooTask(tx *gorm.DB, task db.WooTask) (created, requeued, existed bool, err error) {
	var existing db.WooTask
	switch err = tx.Where("task_key = ?", task.TaskKey).Take(&existing).Error; {
	case err == nil:
		switch existing.Status {
		case "pending", "running":
			return false, false, true, nil
		default:
			updates := map[string]any{
				"import_id":    task.ImportID,
				"towar_id":     task.TowarID,
				"woo_id":       task.WooID,
				"kind":         task.Kind,
				"payload_json": task.PayloadJSON,
				"status":       "pending",
				"attempts":     0,
				"last_error":   "",
				"started_at":   nil,
				"finished_at":  nil,
				"depends_on":   task.DependsOn,
			}
			if err := tx.Model(&db.WooTask{}).Where("task_id = ?", existing.TaskID).Updates(updates).Error; err != nil {
				return false, false, false, err
			}
			return false, true, false, nil
		}
	case errors.Is(err, gorm.ErrRecordNotFound):
		if err := tx.Create(&task).Error; err != nil {
			return false, false, false, err
		}
		return true, false, false, nil
	default:
		return false, false, false, err
	}
}

func buildTaskKey(kind string, wooID uint, parts ...string) string {
	base := []string{kind, strconv.FormatUint(uint64(wooID), 10)}
	base = append(base, parts...)
	return strings.Join(base, ":")
}

func normalizeFloatKey(v float64) string {
	return strconv.FormatFloat(v, 'f', 6, 64)
}

func mustJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func floatAlmostEqual(a, b float64) bool {
	return math.Abs(a-b) < 0.0001
}

func ptrInt64(v int64) *int64 {
	return &v
}

func ptrUint(v uint) *uint {
	return &v
}
