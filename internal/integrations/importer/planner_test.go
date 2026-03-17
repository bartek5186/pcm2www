package importer

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/bartek5186/pcm2www/internal/db"
	"github.com/rs/zerolog"
	"gorm.io/driver/sqlite"
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

func TestPlanWooTasksCreatesEANStockAndPriceTasks(t *testing.T) {
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
		Ean:          "",
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

	if _, ok := gotKinds[db.WooTaskKindEANUpdate]; !ok {
		t.Fatal("missing ean.update task")
	}
	if _, ok := gotKinds[db.WooTaskKindStockUpdate]; !ok {
		t.Fatal("missing stock.update task")
	}
	if _, ok := gotKinds[db.WooTaskKindPriceUpdate]; !ok {
		t.Fatal("missing price.update task")
	}

	var stockPayload db.WooStockUpdatePayload
	if err := json.Unmarshal([]byte(gotKinds[db.WooTaskKindStockUpdate].PayloadJSON), &stockPayload); err != nil {
		t.Fatal(err)
	}
	if stockPayload.DesiredStock != 3 {
		t.Fatalf("expected desired stock 3, got %v", stockPayload.DesiredStock)
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

	var count int64
	if err := gdb.Model(&db.WooTask{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected 0 tasks due to safety policy, got %d", count)
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

func newImporterTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
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
