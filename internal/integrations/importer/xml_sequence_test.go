package importer

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/bartek5186/pcm2www/internal/db"
	"github.com/rs/zerolog"
	"gorm.io/gorm"
)

const realXMLFixtureEnv = "PCM2WWW_IMPORT_XML_FIXTURE_TESTS"

type importSnapshot struct {
	ImportFiles          int64
	Products             int64
	Stocks               int64
	CurrentProducts      int64
	CurrentStocks        int64
	StocksWithPrev       int64
	ChangedCurrentStocks int64
}

func TestImportRealXMLSequenceIntoIsolatedDB(t *testing.T) {
	if os.Getenv(realXMLFixtureEnv) != "1" {
		t.Skipf("set %s=1 to run real imports/*.xml fixture sequence", realXMLFixtureEnv)
	}

	files, err := filepath.Glob(filepath.Join("..", "..", "..", "imports", "exp_wyk_*.xml"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	if len(files) < 2 {
		t.Fatalf("expected at least 2 XML files in imports/, got %d", len(files))
	}

	gdb := newImporterTestDB(t)
	imp := &Importer{log: zerolog.Nop(), db: gdb}

	var first, final importSnapshot
	totalChangedStockRows := int64(0)
	startedAt := time.Now()

	for idx, path := range files {
		name := filepath.Base(path)
		before := snapshotImportDB(t, gdb, 0)

		importID, already, err := imp.registerFile(path, name)
		if err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
		if already {
			t.Fatalf("unexpected duplicate registration for %s in isolated DB", name)
		}

		if err := imp.processFile(importID, path); err != nil {
			t.Fatalf("process %s: %v", name, err)
		}
		now := time.Now()
		if err := gdb.Model(&db.ImportFile{}).
			Where("import_id = ?", importID).
			Updates(map[string]any{"status": 1, "processed_at": now}).Error; err != nil {
			t.Fatalf("mark %s done: %v", name, err)
		}

		after := snapshotImportDB(t, gdb, importID)
		if after.CurrentProducts == 0 {
			t.Fatalf("%s imported no product rows", name)
		}
		if after.CurrentStocks == 0 {
			t.Fatalf("%s imported no stock rows", name)
		}
		if after.Products < before.Products {
			t.Fatalf("%s reduced product row count from %d to %d", name, before.Products, after.Products)
		}
		if after.Stocks < before.Stocks {
			t.Fatalf("%s reduced stock row count from %d to %d", name, before.Stocks, after.Stocks)
		}

		totalChangedStockRows += after.ChangedCurrentStocks
		if idx == 0 {
			first = after
		}
		final = after

		t.Logf(
			"%03d/%03d %s: products=%d(+%d current=%d), stocks=%d(+%d current=%d), stocks_with_prev=%d, changed_current_stocks=%d",
			idx+1,
			len(files),
			name,
			after.Products,
			after.Products-before.Products,
			after.CurrentProducts,
			after.Stocks,
			after.Stocks-before.Stocks,
			after.CurrentStocks,
			after.StocksWithPrev,
			after.ChangedCurrentStocks,
		)
	}

	if first.Products == 0 || first.Stocks == 0 {
		t.Fatalf("first import did not seed staging tables: %+v", first)
	}
	if final.ImportFiles != int64(len(files)) {
		t.Fatalf("expected %d import_files rows, got %d", len(files), final.ImportFiles)
	}
	if final.StocksWithPrev == 0 {
		t.Fatal("expected later imports to populate st_stocks.stan_prev")
	}
	if totalChangedStockRows == 0 {
		t.Fatal("expected at least one stock row to change across cyclic XML imports")
	}
	assertNoDuplicateImportRows(t, gdb)

	lastName := filepath.Base(files[len(files)-1])
	_, already, err := imp.registerFile(files[len(files)-1], lastName)
	if err != nil {
		t.Fatalf("re-register %s: %v", lastName, err)
	}
	if !already {
		t.Fatalf("expected re-registering %s to be deduplicated", lastName)
	}

	t.Logf("imported %d XML files into isolated test DB in %s", len(files), time.Since(startedAt).Round(time.Millisecond))
}

func snapshotImportDB(t *testing.T, gdb *gorm.DB, importID uint) importSnapshot {
	t.Helper()

	var s importSnapshot
	mustCount(t, gdb.Model(&db.ImportFile{}), &s.ImportFiles)
	mustCount(t, gdb.Model(&db.StProduct{}), &s.Products)
	mustCount(t, gdb.Model(&db.StStock{}), &s.Stocks)
	mustCount(t, gdb.Model(&db.StStock{}).Where("stan_prev IS NOT NULL"), &s.StocksWithPrev)

	if importID != 0 {
		mustCount(t, gdb.Model(&db.StProduct{}).Where("import_id = ?", importID), &s.CurrentProducts)
		mustCount(t, gdb.Model(&db.StStock{}).Where("import_id = ?", importID), &s.CurrentStocks)
		mustCount(t,
			gdb.Model(&db.StStock{}).
				Where("import_id = ? AND stan_prev IS NOT NULL AND ABS(stan - stan_prev) > 0.000001", importID),
			&s.ChangedCurrentStocks,
		)
	}

	return s
}

func mustCount(t *testing.T, q *gorm.DB, out *int64) {
	t.Helper()
	if err := q.Count(out).Error; err != nil {
		t.Fatal(err)
	}
}

func assertNoDuplicateImportRows(t *testing.T, gdb *gorm.DB) {
	t.Helper()

	var duplicateProducts int64
	if err := gdb.Raw(`
SELECT COUNT(*) FROM (
	SELECT towar_id, kod
	FROM st_products
	GROUP BY towar_id, kod
	HAVING COUNT(*) > 1
) AS duplicates;
`).Scan(&duplicateProducts).Error; err != nil {
		t.Fatal(err)
	}
	if duplicateProducts != 0 {
		t.Fatalf("found %d duplicate st_products keys", duplicateProducts)
	}

	var duplicateStocks int64
	if err := gdb.Raw(`
SELECT COUNT(*) FROM (
	SELECT towar_id, magazyn_id
	FROM st_stocks
	GROUP BY towar_id, magazyn_id
	HAVING COUNT(*) > 1
) AS duplicates;
`).Scan(&duplicateStocks).Error; err != nil {
		t.Fatal(err)
	}
	if duplicateStocks != 0 {
		t.Fatalf("found %d duplicate st_stocks keys", duplicateStocks)
	}
}
