package importer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/bartek5186/pcm2www/internal/db"
	"github.com/rs/zerolog"
	"gorm.io/gorm"
)

// Every scenario starts with a unique EAN match, so queue failures cannot be
// mistaken for the deliberate policy of skipping unlinked products.
func linkedPlanner(t *testing.T) (*Importer, *gorm.DB) {
	t.Helper()
	gdb := newImporterTestDB(t)
	imp := &Importer{log: zerolog.Nop(), db: gdb}
	for _, row := range []any{
		&db.ImportFile{ImportID: 1, SHA256: "one", Status: 1},
		&db.ImportFile{ImportID: 2, SHA256: "two", Status: 1},
		&db.ImportFile{ImportID: 3, SHA256: "three", Status: 1},
		&db.StProduct{TowarID: 42, ImportID: 1, Kod: "5901234567890", CenaDetal: 10, CenaHurtowa: 8, VatID: 2300},
		&db.StStock{TowarID: 42, MagazynID: 1, Stan: 10},
		&db.WooProductCache{WooID: 900, Ean: "5901234567890", PriceRegular: 10, HurtPrice: 8, TaxClass: "2300", StockQty: 10, StockManaged: true, StockStatus: "instock", Backorders: "notify", CatalogVisibility: "visible"},
	} {
		if err := gdb.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := imp.LinkProductsByEAN(); err != nil {
		t.Fatal(err)
	}
	var cache db.WooProductCache
	if err := gdb.First(&cache).Error; err != nil {
		t.Fatal(err)
	}
	if cache.TowarID == nil || *cache.TowarID != 42 {
		t.Fatal("test must have a unique EAN link")
	}
	return imp, gdb
}

func TestPlannerReturningToCompletedPriceAdvancesRevision(t *testing.T) {
	imp, gdb := linkedPlanner(t)
	var first db.WooTask
	for step, price := range []float64{20, 30, 20} {
		if err := gdb.Model(&db.StProduct{}).Where("towar_id = ?", 42).Updates(map[string]any{"cena_detal": price, "import_id": step + 1}).Error; err != nil {
			t.Fatal(err)
		}
		if err := imp.PlanWooTasks(uint(step + 1)); err != nil {
			t.Fatal(err)
		}
		var task db.WooTask
		if err := gdb.Where("kind = ? AND status = ?", db.WooTaskKindPriceUpdate, "pending").Take(&task).Error; err != nil {
			t.Fatal(err)
		}
		if step == 0 {
			first = task
		}
		if step == 2 {
			if task.TaskID != first.TaskID || task.Revision <= first.Revision+1 {
				t.Fatalf("returning price must reuse key with newer revision: first=%+v returned=%+v", first, task)
			}
			if err := imp.PlanWooTasks(3); err != nil {
				t.Fatal(err)
			}
			var again db.WooTask
			if err := gdb.First(&again, "task_id = ?", task.TaskID).Error; err != nil {
				t.Fatal(err)
			}
			if again.Revision != task.Revision {
				t.Fatal("identical pending intent must remain idempotent")
			}
		}
		if err := gdb.Model(&task).Update("status", "done").Error; err != nil {
			t.Fatal(err)
		}
		if err := gdb.Model(&db.WooProductCache{}).Where("woo_id = ?", 900).Update("price_regular", price).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func TestPlannerCancelsObsoleteIntentWhenSourceReturnsToCache(t *testing.T) {
	for _, kind := range []string{db.WooTaskKindPriceUpdate, db.WooTaskKindStockUpdate, db.WooTaskKindAvailabilityUpdate} {
		for _, status := range []string{"pending", "running"} {
			t.Run(kind+"/"+status, func(t *testing.T) {
				imp, gdb := linkedPlanner(t)
				setSource := func(changed bool, importID uint) {
					t.Helper()
					price, stock := 10.0, 10.0
					if changed {
						switch kind {
						case db.WooTaskKindPriceUpdate:
							price = 20
						case db.WooTaskKindStockUpdate:
							stock = 20
						case db.WooTaskKindAvailabilityUpdate:
							price = 0
						}
					}
					if err := gdb.Model(&db.StProduct{}).Where("towar_id = ?", 42).Updates(map[string]any{"cena_detal": price, "import_id": importID}).Error; err != nil {
						t.Fatal(err)
					}
					if err := gdb.Model(&db.StStock{}).Where("towar_id = ?", 42).Update("stan", stock).Error; err != nil {
						t.Fatal(err)
					}
				}
				setSource(true, 1)
				if err := imp.PlanWooTasks(1); err != nil {
					t.Fatal(err)
				}
				var obsolete db.WooTask
				if err := gdb.Where("kind = ? AND status = ?", kind, "pending").Take(&obsolete).Error; err != nil {
					t.Fatal(err)
				}
				if err := gdb.Model(&obsolete).Update("status", status).Error; err != nil {
					t.Fatal(err)
				}
				setSource(false, 2)
				if err := imp.PlanWooTasks(2); err != nil {
					t.Fatal(err)
				}
				if err := gdb.First(&obsolete, "task_id = ?", obsolete.TaskID).Error; err != nil {
					t.Fatal(err)
				}
				if obsolete.Status != "superseded" {
					t.Fatalf("obsolete %s survives cache equality: %+v", kind, obsolete)
				}
				var active int64
				if err := gdb.Model(&db.WooTask{}).Where("status IN ?", []string{"pending", "running"}).Count(&active).Error; err != nil {
					t.Fatal(err)
				}
				if active != 0 {
					t.Fatalf("unexpected active tasks: %d", active)
				}
			})
		}
	}
}

func TestReservationOnlyXMLChangesPlanAvailableStock(t *testing.T) {
	gdb := newImporterTestDB(t)
	imp := &Importer{log: zerolog.Nop(), db: gdb}
	dir := t.TempDir()
	if err := gdb.Create(&db.WooProductCache{WooID: 900, Ean: "5901234567890", PriceRegular: 10, TaxClass: "2300", StockQty: 10, StockManaged: true, Backorders: "notify", CatalogVisibility: "visible"}).Error; err != nil {
		t.Fatal(err)
	}
	for step, reserved := range []int{0, 3, 1, 1} {
		// Last export follows an online sale: PCM availability stayed at 9,
		// Woo now has 8. The unchanged-source protection must still hold.
		cacheStock := []float64{10, 10, 7, 8}[step]
		if err := gdb.Model(&db.WooProductCache{}).Where("woo_id = ?", 900).Update("stock_qty", cacheStock).Error; err != nil {
			t.Fatal(err)
		}
		name := fmt.Sprintf("exp_wyk_reservation_%d.xml", step)
		writePlanningXML(t, dir, name, fmt.Sprintf("reservation-%d", step), reserved)
		imp.scanOnce(dir)
		row := mustImportFile(t, gdb, name)
		var stock db.StStock
		if err := gdb.First(&stock).Error; err != nil {
			t.Fatal(err)
		}
		if step == 0 && stock.RezerwacjaPrev != nil {
			t.Fatal("first import must have no reservation history")
		}
		if step > 0 && (stock.RezerwacjaPrev == nil || *stock.RezerwacjaPrev != float64([]int{0, 3, 1}[step-1])) {
			t.Fatalf("wrong reservation history: %+v", stock)
		}
		var tasks []db.WooTask
		if err := gdb.Where("import_id = ? AND kind = ? AND status = ?", row.ImportID, db.WooTaskKindStockUpdate, "pending").Find(&tasks).Error; err != nil {
			t.Fatal(err)
		}
		if step == 1 || step == 2 {
			if len(tasks) != 1 {
				t.Fatalf("reservation-only change should plan one stock task, got %+v", tasks)
			}
			var payload db.WooStockUpdatePayload
			if err := gdb.Model(&tasks[0]).Update("status", "done").Error; err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tasks[0].PayloadJSON), &payload); err != nil {
				t.Fatal(err)
			}
			if payload.DesiredStock != float64(10-reserved) {
				t.Fatalf("wrong available stock: %+v", payload)
			}
		} else if len(tasks) != 0 {
			t.Fatalf("unchanged stock should not be written: %+v", tasks)
		}
	}
}

func writePlanningXML(t *testing.T, dir, name, transmission string, reserved int) {
	t.Helper()
	raw := fmt.Sprintf(`<root><transmisja_id>%s</transmisja_id><towary><towar><towar_id>42</towar_id><kod>5901234567890</kod><nazwa>Linked product</nazwa><vat_id>2300</vat_id><cena_detal>10</cena_detal><magazyny><magazyn><magazyn_id>1</magazyn_id><stan_magazynu>10</stan_magazynu><rezerwacja_ilosci>%d</rezerwacja_ilosci></magazyn></magazyny></towar></towary></root>`, transmission, reserved)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestArchivedImportRetriesFailedLinkOrPlanWithoutNewXML(t *testing.T) {
	for _, stage := range []string{"link", "plan", "completion"} {
		t.Run(stage, func(t *testing.T) {
			appDir := t.TempDir()
			handle, err := db.OpenAt(appDir)
			if err != nil {
				t.Fatal(err)
			}
			gdb := handle.DB
			sqlDB, err := gdb.DB()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := sqlDB.Close(); err != nil {
					t.Errorf("close reopened DB: %v", err)
				}
			})
			if err := handle.Migrate(); err != nil {
				t.Fatal(err)
			}
			imp := &Importer{log: zerolog.Nop(), db: gdb}
			dir := t.TempDir()
			if err := gdb.Create(&db.WooProductCache{WooID: 900, Ean: "5901234567890", PriceRegular: 20, StockQty: 10, StockManaged: true, Backorders: "notify", CatalogVisibility: "visible"}).Error; err != nil {
				t.Fatal(err)
			}
			callback := "test:fail-planning"
			failuresInjected := 0
			fail := func(tx *gorm.DB) {
				if (stage == "link" && tx.Statement.Table == "link_issues") || (stage == "plan" && tx.Statement.Table == "woo_tasks") {
					failuresInjected++
					tx.AddError(errors.New("injected planning failure"))
				}
				if stage == "completion" && tx.Statement.Table == "import_files" {
					if values, ok := tx.Statement.Dest.(map[string]any); ok && values["planning_pending"] == false {
						failuresInjected++
						tx.AddError(errors.New("injected completion failure"))
					}
				}
			}
			switch stage {
			case "link":
				if err := gdb.Callback().Delete().Before("gorm:delete").Register(callback, fail); err != nil {
					t.Fatal(err)
				}
			case "plan":
				if err := gdb.Callback().Create().Before("gorm:create").Register(callback, fail); err != nil {
					t.Fatal(err)
				}
			case "completion":
				if err := gdb.Callback().Update().Before("gorm:update").Register(callback, fail); err != nil {
					t.Fatal(err)
				}
			}
			name := "exp_wyk_retry.xml"
			writePlanningXML(t, dir, name, "retry", 0)
			imp.scanOnce(dir)
			if failuresInjected == 0 {
				t.Fatal("test did not reach the intended failure injection")
			}
			row := mustImportFile(t, gdb, name)
			if row.Status != 1 || !row.PlanningPending {
				t.Fatalf("committed import must remain pending planning: %+v", row)
			}
			assertArchivedXML(t, dir, name)
			var taskCount int64
			mustCount(t, gdb.Model(&db.WooTask{}), &taskCount)
			if taskCount != 0 {
				t.Fatal("failed planning transaction left partial tasks")
			}
			switch stage {
			case "link":
				if err := gdb.Callback().Delete().Remove(callback); err != nil {
					t.Fatal(err)
				}
			case "plan":
				if err := gdb.Callback().Create().Remove(callback); err != nil {
					t.Fatal(err)
				}
			case "completion":
				if err := gdb.Callback().Update().Remove(callback); err != nil {
					t.Fatal(err)
				}
			}
			// Close the connection and reopen the file: the retry must survive
			// loss of both the importer and its original database connection.
			if err := sqlDB.Close(); err != nil {
				t.Fatal(err)
			}
			handle, err = db.OpenAt(appDir)
			if err != nil {
				t.Fatal(err)
			}
			gdb = handle.DB
			sqlDB, err = gdb.DB()
			if err != nil {
				t.Fatal(err)
			}
			row = mustImportFile(t, gdb, name)
			if !row.PlanningPending {
				t.Fatal("retry request was not persisted on disk")
			}
			imp = &Importer{log: zerolog.Nop(), db: gdb}
			imp.scanOnce(dir)
			row = mustImportFile(t, gdb, name)
			if row.PlanningPending {
				t.Fatal("successful retry did not clear durable request")
			}
			var task db.WooTask
			if err := gdb.Where("kind = ? AND status = ?", db.WooTaskKindPriceUpdate, "pending").Take(&task).Error; err != nil {
				t.Fatal(err)
			}
			if task.TowarID == nil || *task.TowarID != 42 || task.WooID == nil || *task.WooID != 900 {
				t.Fatalf("retry did not use the EAN link: %+v", task)
			}
			imp.scanOnce(dir)
			var after int64
			mustCount(t, gdb.Model(&db.WooTask{}), &after)
			if after != 1 {
				t.Fatalf("retry generated duplicate tasks: %d", after)
			}
		})
	}
}

func TestSuccessfulPlanningWithoutEANMatchClearsRetryRequest(t *testing.T) {
	gdb := newImporterTestDB(t)
	imp := &Importer{log: zerolog.Nop(), db: gdb}
	dir := t.TempDir()
	if err := gdb.Create(&db.WooProductCache{WooID: 900, Ean: "9999999999999"}).Error; err != nil {
		t.Fatal(err)
	}
	writePlanningXML(t, dir, "exp_wyk_unlinked.xml", "unlinked", 0)
	imp.scanOnce(dir)
	row := mustImportFile(t, gdb, "exp_wyk_unlinked.xml")
	if row.Status != 1 || row.PlanningPending {
		t.Fatalf("missing link is not a planning failure: %+v", row)
	}
	var tasks int64
	mustCount(t, gdb.Model(&db.WooTask{}), &tasks)
	if tasks != 0 {
		t.Fatal("unlinked product generated tasks")
	}
	var issues int64
	mustCount(t, gdb.Model(&db.LinkIssue{}).Where("towar_id = ?", 42), &issues)
	if issues == 0 {
		t.Fatal("missing EAN match should leave diagnostics")
	}
}

func TestPlanningFailurePreservesStockChangeBeforeNextXML(t *testing.T) {
	gdb := newImporterTestDB(t)
	imp := &Importer{log: zerolog.Nop(), db: gdb}
	dir := t.TempDir()
	if err := gdb.Create(&db.WooProductCache{WooID: 900, Ean: "5901234567890", PriceRegular: 10, TaxClass: "2300", StockQty: 10, StockManaged: true, Backorders: "notify", CatalogVisibility: "visible"}).Error; err != nil {
		t.Fatal(err)
	}
	writePlanningXML(t, dir, "exp_wyk_1.xml", "before", 0)
	imp.scanOnce(dir)
	if err := gdb.Callback().Create().Before("gorm:create").Register("test:stock-failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "woo_tasks" {
			tx.AddError(errors.New("queue unavailable"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	writePlanningXML(t, dir, "exp_wyk_2.xml", "changed", 3)
	imp.scanOnce(dir)
	// A repeated snapshot must not erase the 0 -> 3 reservation change while
	// planning is still failing. It remains on disk until the queue recovers.
	writePlanningXML(t, dir, "exp_wyk_3.xml", "unchanged", 3)
	imp.scanOnce(dir)
	assertFileExists(t, filepath.Join(dir, "exp_wyk_3.xml"))
	var stock db.StStock
	if err := gdb.First(&stock).Error; err != nil {
		t.Fatal(err)
	}
	if stock.RezerwacjaPrev == nil || *stock.RezerwacjaPrev != 0 {
		t.Fatalf("unplanned change was overwritten: %+v", stock)
	}
	if err := gdb.Callback().Create().Remove("test:stock-failure"); err != nil {
		t.Fatal(err)
	}
	imp.scanOnce(dir)
	assertArchivedXML(t, dir, "exp_wyk_3.xml")
	var tasks []db.WooTask
	if err := gdb.Where("kind = ? AND status = ?", db.WooTaskKindStockUpdate, "pending").Find(&tasks).Error; err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("stock change was lost during retry: %+v", tasks)
	}
	var payload db.WooStockUpdatePayload
	if err := json.Unmarshal([]byte(tasks[0].PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.DesiredStock != 7 {
		t.Fatalf("retry has wrong stock: %+v", payload)
	}
}

func TestPlannerRefreshesLegacyPendingPriceAgainstCompletedHistory(t *testing.T) {
	imp, gdb := linkedPlanner(t)
	if err := gdb.Model(&db.StProduct{}).Where("towar_id = ?", 42).Update("cena_detal", 20).Error; err != nil {
		t.Fatal(err)
	}
	old := db.WooTask{TaskKey: buildTaskKey(db.WooTaskKindPriceUpdate, 900, normalizeFloatKey(20), normalizeFloatKey(8), "2300"), Kind: db.WooTaskKindPriceUpdate, WooID: ptrUint(900), TowarID: ptrInt64(42), Status: "pending"}
	if err := gdb.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&db.WooTask{TaskKey: "legacy-completed-price", Kind: db.WooTaskKindPriceUpdate, WooID: ptrUint(900), TowarID: ptrInt64(42), Status: "done"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := imp.PlanWooTasks(1); err != nil {
		t.Fatal(err)
	}
	var current db.WooTask
	if err := gdb.First(&current, "task_id = ?", old.TaskID).Error; err != nil {
		t.Fatal(err)
	}
	if current.Status != "pending" || current.Revision == 0 {
		t.Fatalf("legacy pending task still uses obsolete ID order: %+v", current)
	}
	var payload db.WooPriceUpdatePayload
	if err := json.Unmarshal([]byte(current.PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.DesiredRegular != 20 {
		t.Fatalf("legacy payload not refreshed from linked staging: %+v", payload)
	}
}
