package importer

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/bartek5186/pcm2www/internal/db"
	"github.com/glebarez/sqlite"
	"github.com/rs/zerolog"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func TestLinkProductsByEANClearsStaleTowarID(t *testing.T) {
	gdb := newImporterTestDB(t)
	importer := &Importer{log: zerolog.Nop(), db: gdb}

	staleID := int64(999)
	matchedOld := int64(888)
	if err := gdb.Create(&db.WooProductCache{
		WooID:   1,
		TowarID: &staleID,
		Kod:     "STALE",
		Ean:     "1234567890123",
		Name:    "Stale product",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&db.WooProductCache{
		WooID:   2,
		TowarID: &matchedOld,
		Kod:     "MATCH",
		Ean:     "5901234567890",
		Name:    "Matched product",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&db.StProduct{
		ImportID: 1,
		TowarID:  42,
		Kod:      "5901234567890",
		Nazwa:    "Source product",
	}).Error; err != nil {
		t.Fatal(err)
	}

	if err := importer.LinkProductsByEAN(); err != nil {
		t.Fatal(err)
	}

	var stale db.WooProductCache
	if err := gdb.Where("woo_id = ?", 1).Take(&stale).Error; err != nil {
		t.Fatal(err)
	}
	if stale.TowarID != nil {
		t.Fatalf("expected stale link to be cleared, got %v", *stale.TowarID)
	}

	var matched db.WooProductCache
	if err := gdb.Where("woo_id = ?", 2).Take(&matched).Error; err != nil {
		t.Fatal(err)
	}
	if matched.TowarID == nil || *matched.TowarID != 42 {
		t.Fatalf("expected matched link to be rebuilt, got %+v", matched.TowarID)
	}
}

func TestLinkProductsByEANRejectsDuplicateSourceEAN(t *testing.T) {
	gdb := newImporterTestDB(t)
	importer := &Importer{log: zerolog.Nop(), db: gdb}

	if err := gdb.Create([]db.StProduct{
		{ImportID: 1, TowarID: 41, Kod: "590-123-456-7890", Nazwa: "First"},
		{ImportID: 1, TowarID: 42, Kod: "5901234567890", Nazwa: "Second"},
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&db.WooProductCache{WooID: 10, Ean: "5901234567890", Name: "Shop product"}).Error; err != nil {
		t.Fatal(err)
	}

	if err := importer.LinkProductsByEAN(); err != nil {
		t.Fatal(err)
	}

	var cached db.WooProductCache
	if err := gdb.Where("woo_id = ?", 10).Take(&cached).Error; err != nil {
		t.Fatal(err)
	}
	if cached.TowarID != nil {
		t.Fatalf("ambiguous source EAN must not link to Woo, got towar_id=%d", *cached.TowarID)
	}
	var issues []db.LinkIssue
	if err := gdb.Where("reason = ?", "duplicate_ean_source").Order("towar_id").Find(&issues).Error; err != nil {
		t.Fatal(err)
	}
	if len(issues) != 2 || issues[0].TowarID != 41 || issues[1].TowarID != 42 {
		t.Fatalf("expected diagnostics for both duplicate source products, got %+v", issues)
	}
}

func TestPlanWooTasksCreatesStockPriceAndAvailabilityTasks(t *testing.T) {
	gdb := newImporterTestDB(t)
	importer := &Importer{log: zerolog.Nop(), db: gdb}

	const importID = 7
	towarID := int64(100)
	wooID := uint(200)

	if err := gdb.Create(&db.ImportFile{ImportID: importID, Filename: "exp_wyk_test.xml", Status: 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&db.StProduct{
		ImportID:    importID,
		TowarID:     towarID,
		Kod:         "5901234567890",
		Nazwa:       "Test Product",
		CenaDetal:   25,
		CenaHurtowa: 17,
		AktywnyWSI:  true,
		DoUsuniecia: false,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create([]db.StStock{
		{ImportID: importID, TowarID: towarID, MagazynID: 1, Stan: 5, Rezerwacja: 2},
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&db.WooProductCache{
		WooID:        wooID,
		TowarID:      &towarID,
		Kod:          "SKU-1",
		Ean:          "5901234567890",
		Name:         "Test Product",
		PriceRegular: 20,
		PriceSale:    0,
		HurtPrice:    15,
		StockQty:     10,
		StockManaged: true,
	}).Error; err != nil {
		t.Fatal(err)
	}

	if err := importer.PlanWooTasks(importID); err != nil {
		t.Fatal(err)
	}
	if err := importer.PlanWooTasks(importID); err != nil {
		t.Fatal(err)
	}

	var tasks []db.WooTask
	if err := gdb.Order("kind asc").Find(&tasks).Error; err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 3 {
		t.Fatalf("expected 3 tasks, got %d", len(tasks))
	}

	gotKinds := make(map[string]db.WooTask, len(tasks))
	for _, task := range tasks {
		gotKinds[task.Kind] = task
		if task.Status != "pending" {
			t.Fatalf("expected pending task, got %s", task.Status)
		}
		if task.WooID == nil || *task.WooID != wooID {
			t.Fatalf("unexpected WooID on task %+v", task)
		}
		if task.TowarID == nil || *task.TowarID != towarID {
			t.Fatalf("unexpected TowarID on task %+v", task)
		}
	}

	if _, ok := gotKinds[db.WooTaskKindEANUpdate]; ok {
		t.Fatal("planner must not create ean.update tasks; EAN is the link key")
	}
	if _, ok := gotKinds[db.WooTaskKindStockUpdate]; !ok {
		t.Fatal("missing stock.update task")
	}
	if _, ok := gotKinds[db.WooTaskKindPriceUpdate]; !ok {
		t.Fatal("missing price.update task")
	}
	if _, ok := gotKinds[db.WooTaskKindAvailabilityUpdate]; !ok {
		t.Fatal("missing availability.update task")
	}

	var stockPayload db.WooStockUpdatePayload
	if err := json.Unmarshal([]byte(gotKinds[db.WooTaskKindStockUpdate].PayloadJSON), &stockPayload); err != nil {
		t.Fatal(err)
	}
	if stockPayload.DesiredStock != 3 {
		t.Fatalf("expected desired stock 3, got %v", stockPayload.DesiredStock)
	}
}

func TestPlanWooTasksCanSendNetPrices(t *testing.T) {
	gdb := newImporterTestDB(t)
	importer := &Importer{log: zerolog.Nop(), db: gdb, cfg: Config{PriceMode: priceModeNet}}

	const importID = 8
	towarID := int64(110)
	wooID := uint(210)

	if err := gdb.Create(&db.ImportFile{ImportID: importID, Filename: "exp_wyk_net_prices.xml", Status: 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&db.StProduct{
		ImportID:    importID,
		TowarID:     towarID,
		Kod:         "5901234567891",
		Nazwa:       "Net Price Product",
		VatID:       2300,
		CenaDetal:   123,
		CenaHurtowa: 61.5,
		AktywnyWSI:  true,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&db.WooProductCache{
		WooID:        wooID,
		TowarID:      &towarID,
		Kod:          "SKU-NET",
		Ean:          "5901234567891",
		Name:         "Net Price Product",
		PriceRegular: 123,
		PriceSale:    0,
		HurtPrice:    61.5,
		StockManaged: true,
		Backorders:   "notify",
	}).Error; err != nil {
		t.Fatal(err)
	}

	if err := importer.PlanWooTasks(importID); err != nil {
		t.Fatal(err)
	}

	var task db.WooTask
	if err := gdb.Where("kind = ?", db.WooTaskKindPriceUpdate).Take(&task).Error; err != nil {
		t.Fatal(err)
	}
	var pricePayload db.WooPriceUpdatePayload
	if err := json.Unmarshal([]byte(task.PayloadJSON), &pricePayload); err != nil {
		t.Fatal(err)
	}
	if pricePayload.DesiredRegular != 100 {
		t.Fatalf("expected net regular price 100, got %v", pricePayload.DesiredRegular)
	}
	if pricePayload.DesiredHurt != 50 {
		t.Fatalf("expected net hurt price 50, got %v", pricePayload.DesiredHurt)
	}
	if pricePayload.DesiredTaxClass != "2300" {
		t.Fatalf("expected tax class 2300, got %q", pricePayload.DesiredTaxClass)
	}
}

func TestPlanWooTasksAppliesSafetyPolicy(t *testing.T) {
	gdb := newImporterTestDB(t)
	importer := &Importer{log: zerolog.Nop(), db: gdb}

	const importID = 9
	towarID := int64(101)
	wooID := uint(201)
	otherWooID := uint(202)

	if err := gdb.Create(&db.ImportFile{ImportID: importID, Filename: "exp_wyk_test_2.xml", Status: 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&db.StProduct{
		ImportID:    importID,
		TowarID:     towarID,
		Kod:         "5901234567890",
		Nazwa:       "Policy Product",
		CenaDetal:   30,
		CenaHurtowa: 20,
		AktywnyWSI:  true,
		DoUsuniecia: false,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&db.StStock{ImportID: importID, TowarID: towarID, MagazynID: 1, Stan: 7, Rezerwacja: 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&db.WooProductCache{
		WooID:        wooID,
		TowarID:      &towarID,
		Kod:          "SKU-2",
		Ean:          "1111111111111",
		Name:         "Policy Product",
		PriceRegular: 10,
		PriceSale:    2,
		HurtPrice:    10,
		StockQty:     1,
		StockManaged: false,
		Backorders:   "",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&db.WooProductCache{
		WooID: otherWooID,
		Kod:   "OTHER",
		Ean:   "5901234567890",
		Name:  "Duplicate owner",
	}).Error; err != nil {
		t.Fatal(err)
	}

	if err := importer.PlanWooTasks(importID); err != nil {
		t.Fatal(err)
	}

	var tasks []db.WooTask
	if err := gdb.Find(&tasks).Error; err != nil {
		t.Fatal(err)
	}
	// Stock/price są zablokowane przez polityki bezpieczeństwa.
	// availability.update jest słuszny — cache ma manage_stock=false przy cenie > 0.
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task (availability.update), got %d", len(tasks))
	}
	if tasks[0].Kind != db.WooTaskKindAvailabilityUpdate {
		t.Fatalf("expected availability.update task, got %s", tasks[0].Kind)
	}
	var availabilityPayload db.WooAvailabilityPayload
	if err := json.Unmarshal([]byte(tasks[0].PayloadJSON), &availabilityPayload); err != nil {
		t.Fatal(err)
	}
	if !availabilityPayload.SetStock || availabilityPayload.DesiredStock != 6 {
		t.Fatalf("reactivation must restore stock 6, got %+v", availabilityPayload)
	}
}

func TestEANOnlyFlowDoesNotPlanTasksWithoutMatchingEAN(t *testing.T) {
	gdb := newImporterTestDB(t)
	imp := &Importer{log: zerolog.Nop(), db: gdb}
	const importID = 12

	if err := gdb.Create(&db.ImportFile{ImportID: importID, Filename: "exp_wyk_ean_only.xml", Status: 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&db.StProduct{ImportID: importID, TowarID: 700, Kod: "5901234567890", CenaDetal: 10}).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&db.WooProductCache{WooID: 800, Kod: "SKU-800", Ean: "", Name: "No EAN"}).Error; err != nil {
		t.Fatal(err)
	}

	if err := imp.LinkProductsByEAN(); err != nil {
		t.Fatal(err)
	}
	if err := imp.PlanWooTasks(importID); err != nil {
		t.Fatal(err)
	}

	var cache db.WooProductCache
	if err := gdb.Where("woo_id = ?", 800).Take(&cache).Error; err != nil {
		t.Fatal(err)
	}
	if cache.TowarID != nil {
		t.Fatalf("product without matching EAN must remain unlinked, got %v", *cache.TowarID)
	}
	var count int64
	if err := gdb.Model(&db.WooTask{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("unlinked product must not produce tasks, got %d", count)
	}
}

func TestDeletedSourceOnlyPlansUnavailability(t *testing.T) {
	t.Run("marked_for_deletion", func(t *testing.T) {
		gdb := newImporterTestDB(t)
		imp := &Importer{log: zerolog.Nop(), db: gdb}
		const importID = 31
		towarID := int64(731)
		wooID := uint(831)
		if err := gdb.Create(&db.ImportFile{ImportID: importID, Filename: "exp_wyk_policy_deleted.xml", Status: 1}).Error; err != nil {
			t.Fatal(err)
		}
		if err := gdb.Create(&db.StProduct{
			ImportID: importID, TowarID: towarID, Kod: "5901234567000", CenaDetal: 25, CenaHurtowa: 17,
			AktywnyWSI: false, DoUsuniecia: true,
		}).Error; err != nil {
			t.Fatal(err)
		}
		if err := gdb.Create(&db.StStock{ImportID: importID, TowarID: towarID, MagazynID: 1, Stan: 5}).Error; err != nil {
			t.Fatal(err)
		}
		if err := gdb.Create(&db.WooProductCache{
			WooID: wooID, TowarID: &towarID, Ean: "5901234567000", PriceRegular: 20, HurtPrice: 10,
			StockManaged: true, StockQty: 1, StockStatus: "instock", Backorders: "notify", CatalogVisibility: "visible",
		}).Error; err != nil {
			t.Fatal(err)
		}
		if err := gdb.Create([]db.WooTask{
			{TaskKey: "stock.update:831:9", ImportID: importID, TowarID: &towarID, WooID: &wooID, Kind: db.WooTaskKindStockUpdate, Status: "pending"},
			{TaskKey: "price.update:831:99", ImportID: importID, TowarID: &towarID, WooID: &wooID, Kind: db.WooTaskKindPriceUpdate, Status: "pending"},
		}).Error; err != nil {
			t.Fatal(err)
		}

		if err := imp.PlanWooTasks(importID); err != nil {
			t.Fatal(err)
		}
		var tasks []db.WooTask
		if err := gdb.Find(&tasks).Error; err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 3 {
			t.Fatalf("expected two superseded tasks and one unavailability task, got %+v", tasks)
		}
		var availability db.WooTask
		for _, task := range tasks {
			if task.Kind == db.WooTaskKindAvailabilityUpdate {
				availability = task
				continue
			}
			if task.Status != "superseded" {
				t.Fatalf("inactive source left stale write pending: %+v", task)
			}
		}
		if availability.TaskID == 0 || availability.Status != "pending" {
			t.Fatalf("missing pending unavailability task: %+v", tasks)
		}
		var payload db.WooAvailabilityPayload
		if err := json.Unmarshal([]byte(availability.PayloadJSON), &payload); err != nil {
			t.Fatal(err)
		}
		if !payload.Unavailable || payload.SetStock {
			t.Fatalf("unexpected inactive-source payload: %+v", payload)
		}
	})
}

func TestAktywnyWSIDoesNotGateSynchronization(t *testing.T) {
	if !sourceProductEligible(plannerSourceRow{AktywnyWSI: false, DoUsuniecia: false}) {
		t.Fatal("aktywny_w_SI=N must not disable products without confirmed PC-Market semantics")
	}
	if sourceProductEligible(plannerSourceRow{AktywnyWSI: true, DoUsuniecia: true}) {
		t.Fatal("do_usuniecia=Y must disable the product regardless of aktywny_w_SI")
	}
}

// TestPlanWooTasksSkipsStockWhenPCMUnchanged weryfikuje, że jeśli stan PCM się nie zmienił
// (delta=0), planner nie generuje stock.update — nawet gdy cache Woo różni się (np. po sprzedaży).
func TestPlanWooTasksSkipsStockWhenPCMUnchanged(t *testing.T) {
	gdb := newImporterTestDB(t)
	importer := &Importer{log: zerolog.Nop(), db: gdb}

	const importID = 11
	towarID := int64(300)
	wooID := uint(400)
	prevStan := float64(5)

	if err := gdb.Create(&db.ImportFile{ImportID: importID, Filename: "exp_wyk_test_3.xml", Status: 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&db.StProduct{
		ImportID:    importID,
		TowarID:     towarID,
		Kod:         "4006381333931",
		Nazwa:       "Unchanged Stock Product",
		CenaDetal:   10,
		CenaHurtowa: 8,
		AktywnyWSI:  true,
	}).Error; err != nil {
		t.Fatal(err)
	}
	// Stan=5, StanPrev=5 — PCM nie zmienił stanu od poprzedniego importu
	if err := gdb.Create(&db.StStock{
		ImportID:   importID,
		TowarID:    towarID,
		MagazynID:  1,
		Stan:       5,
		StanPrev:   &prevStan,
		Rezerwacja: 0,
	}).Error; err != nil {
		t.Fatal(err)
	}
	// Cache Woo pokazuje 4 — bo sklep sprzedał 1 sztukę
	if err := gdb.Create(&db.WooProductCache{
		WooID:        wooID,
		TowarID:      &towarID,
		Kod:          "SKU-3",
		Ean:          "4006381333931",
		Name:         "Unchanged Stock Product",
		PriceRegular: 10,
		PriceSale:    0,
		HurtPrice:    8,
		StockQty:     4,
		StockManaged: true,
	}).Error; err != nil {
		t.Fatal(err)
	}

	if err := importer.PlanWooTasks(importID); err != nil {
		t.Fatal(err)
	}

	var stockTasks []db.WooTask
	if err := gdb.Where("kind = ?", db.WooTaskKindStockUpdate).Find(&stockTasks).Error; err != nil {
		t.Fatal(err)
	}
	if len(stockTasks) != 0 {
		t.Fatalf("expected 0 stock tasks (PCM unchanged), got %d", len(stockTasks))
	}
}

func TestEnqueueWooTaskSupersedesOlderPendingState(t *testing.T) {
	gdb := newImporterTestDB(t)
	wooID := uint(900)
	old := db.WooTask{
		TaskKey: "stock.update:900:1", WooID: &wooID, Kind: db.WooTaskKindStockUpdate, Status: "pending",
	}
	if err := gdb.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	newer := db.WooTask{
		TaskKey: "stock.update:900:2", WooID: &wooID, Kind: db.WooTaskKindStockUpdate, Status: "pending",
	}
	created, _, _, err := enqueueWooTask(gdb, newer)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("expected newer desired state to create a task")
	}

	var tasks []db.WooTask
	if err := gdb.Order("task_id ASC").Find(&tasks).Error; err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 || tasks[0].Status != "superseded" || tasks[1].Status != "pending" {
		t.Fatalf("unexpected supersession state: %+v", tasks)
	}
}

func newImporterTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close test DB: %v", err)
		}
	})
	if err := gdb.AutoMigrate(
		&db.ImportFile{},
		&db.StProduct{},
		&db.StStock{},
		&db.WooProductCache{},
		&db.WooTask{},
		&db.KV{},
		&db.LinkIssue{},
	); err != nil {
		t.Fatal(err)
	}
	return gdb
}
