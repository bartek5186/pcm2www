package importer

import (
	"bufio"
	"encoding/json"
	"encoding/xml"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bartek5186/pcm2www/internal/db"
	"github.com/rs/zerolog"
	"golang.org/x/net/html/charset"
	"gorm.io/gorm"
)

const (
	realXMLFixtureEnv      = "PCM2WWW_IMPORT_XML_FIXTURE_TESTS"
	xmlCheckpointEveryEnv  = "PCM2WWW_IMPORT_XML_CHECKPOINT_EVERY"
	defaultCheckpointEvery = 10
)

type importSnapshot struct {
	ImportFiles          int64
	Products             int64
	Stocks               int64
	CurrentProducts      int64
	CurrentStocks        int64
	StocksWithPrev       int64
	ChangedCurrentStocks int64
}

type productKey struct {
	TowarID int64
	Kod     string
}

type stockKey struct {
	TowarID   int64
	MagazynID int64
}

type expectedProduct struct {
	ImportID    uint
	TowarID     int64
	Kod         string
	Nazwa       string
	VatID       int64
	CenaDetal   float64
	CenaHurtowa float64
	AktywnyWSI  bool
	DoUsuniecia bool
}

type expectedStock struct {
	ImportID   uint
	TowarID    int64
	MagazynID  int64
	Stan       float64
	StanPrev   *float64
	Rezerwacja float64
}

type expectedImportState struct {
	Products map[productKey]expectedProduct
	Stocks   map[stockKey]expectedStock
}

func TestImportRealXMLSequenceIntoIsolatedDB(t *testing.T) {
	if os.Getenv(realXMLFixtureEnv) != "1" {
		t.Skipf("set %s=1 to run real imports/*.xml fixture sequence", realXMLFixtureEnv)
	}

	files := realXMLFixtureFiles(t)
	if len(files) < 2 {
		t.Fatalf("expected at least 2 XML fixture files, got %d", len(files))
	}

	gdb := newImporterTestDB(t)
	watchDir := t.TempDir()
	imp := &Importer{log: zerolog.Nop(), db: gdb}
	expected := expectedImportState{
		Products: make(map[productKey]expectedProduct),
		Stocks:   make(map[stockKey]expectedStock),
	}
	checkpointEvery := xmlCheckpointEvery(t)

	var first, final importSnapshot
	totalChangedStockRows := int64(0)
	grossPricePlanned := false
	startedAt := time.Now()

	for idx, sourcePath := range files {
		name := filepath.Base(sourcePath)
		expectedProducts, expectedStocks := parseExpectedXMLFile(t, sourcePath)
		before := snapshotImportDB(t, gdb, 0)

		destPath := filepath.Join(watchDir, name)
		copyTestFile(t, sourcePath, destPath)
		imp.scanOnce(watchDir)

		importFile := mustImportFile(t, gdb, name)
		if importFile.Status != 1 {
			t.Fatalf("%s should be imported successfully, got status=%d error=%q", name, importFile.Status, importFile.LastError)
		}

		applyExpectedImport(&expected, importFile.ImportID, expectedProducts, expectedStocks)
		after := snapshotImportDB(t, gdb, importFile.ImportID)
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

		if !grossPricePlanned {
			grossPricePlanned = assertPlannerUsesGrossStagingPrice(t, gdb, imp, importFile.ImportID)
		}

		if shouldCheckXMLState(idx, len(files), checkpointEvery) {
			assertExpectedImportState(t, gdb, expected, idx+1, name)
		}

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
	if !grossPricePlanned {
		t.Fatal("expected at least one real XML row to produce a gross price.update task")
	}
	assertNoDuplicateImportRows(t, gdb)

	lastName := filepath.Base(files[len(files)-1])
	lastID, already, err := imp.registerFile(filepath.Join(watchDir, lastName), lastName)
	if err != nil {
		t.Fatalf("re-register %s: %v", lastName, err)
	}
	if !already {
		t.Fatalf("expected re-registering %s to be deduplicated", lastName)
	}
	if lastID == 0 {
		t.Fatalf("expected deduplicated %s to return an import id", lastName)
	}

	t.Logf("imported %d XML files into isolated test DB in %s", len(files), time.Since(startedAt).Round(time.Millisecond))
}

func TestImportBrokenXMLDoesNotLeavePartialStagingRows(t *testing.T) {
	gdb := newImporterTestDB(t)
	watchDir := t.TempDir()
	imp := &Importer{log: zerolog.Nop(), db: gdb}

	brokenName := "exp_wyk_9999_20260615120000.xml"
	brokenPath := filepath.Join(watchDir, brokenName)
	if err := os.WriteFile(brokenPath, []byte(`<?xml version="1.0" encoding="UTF-8"?><root><towary><towar><towar_id>1</towar_id>`), 0o600); err != nil {
		t.Fatal(err)
	}

	imp.scanOnce(watchDir)

	brokenImport := mustImportFile(t, gdb, brokenName)
	if brokenImport.Status != 2 {
		t.Fatalf("broken XML should be marked error, got status=%d", brokenImport.Status)
	}
	if strings.TrimSpace(brokenImport.LastError) == "" {
		t.Fatal("broken XML should store last_error")
	}

	var products, stocks int64
	mustCount(t, gdb.Model(&db.StProduct{}), &products)
	mustCount(t, gdb.Model(&db.StStock{}), &stocks)
	if products != 0 || stocks != 0 {
		t.Fatalf("broken XML left partial staging rows: products=%d stocks=%d", products, stocks)
	}

	fixture := firstRealXMLFixtureFile(t)
	goodName := filepath.Base(fixture)
	copyTestFile(t, fixture, filepath.Join(watchDir, goodName))
	imp.scanOnce(watchDir)

	goodImport := mustImportFile(t, gdb, goodName)
	if goodImport.Status != 1 {
		t.Fatalf("valid XML after broken file should import successfully, got status=%d error=%q", goodImport.Status, goodImport.LastError)
	}
	mustCount(t, gdb.Model(&db.StProduct{}), &products)
	mustCount(t, gdb.Model(&db.StStock{}), &stocks)
	if products == 0 || stocks == 0 {
		t.Fatalf("valid XML after broken file did not seed staging: products=%d stocks=%d", products, stocks)
	}
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

func realXMLFixtureFiles(t *testing.T) []string {
	t.Helper()

	roots := []string{
		filepath.Join("..", "..", "..", "imports", "incoming_test", "exp_wyk_*.xml"),
		filepath.Join("..", "..", "..", "imports", "exp_wyk_*.xml"),
	}
	for _, pattern := range roots {
		files, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(files)
		if len(files) > 0 {
			return files
		}
	}
	return nil
}

func firstRealXMLFixtureFile(t *testing.T) string {
	t.Helper()
	files := realXMLFixtureFiles(t)
	if len(files) == 0 {
		t.Fatalf("expected at least one XML fixture file")
	}
	return files[0]
}

func xmlCheckpointEvery(t *testing.T) int {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(xmlCheckpointEveryEnv))
	if raw == "" {
		return defaultCheckpointEvery
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		t.Fatalf("%s must be a positive integer, got %q", xmlCheckpointEveryEnv, raw)
	}
	return n
}

func shouldCheckXMLState(idx, total, every int) bool {
	return idx == 0 || idx == total-1 || (idx+1)%every == 0
}

func parseExpectedXMLFile(t *testing.T, path string) ([]expectedProduct, []expectedStock) {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	dec := xml.NewDecoder(bufio.NewReader(f))
	dec.CharsetReader = func(cs string, in io.Reader) (io.Reader, error) {
		return charset.NewReaderLabel(normalizeCharset(cs), in)
	}

	var products []expectedProduct
	var stocks []expectedStock
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("parse expected %s: %v", filepath.Base(path), err)
		}

		se, ok := tok.(xml.StartElement)
		if !ok || !strings.EqualFold(se.Name.Local, "towary") {
			continue
		}
		var tw struct {
			Items []xmlTowar `xml:"towar"`
		}
		if err := dec.DecodeElement(&tw, &se); err != nil {
			t.Fatalf("parse expected towary %s: %v", filepath.Base(path), err)
		}
		for _, row := range tw.Items {
			kod := strings.TrimSpace(row.Kod)
			products = append(products, expectedProduct{
				TowarID:     row.TowarID,
				Kod:         kod,
				Nazwa:       strings.TrimSpace(row.Nazwa),
				VatID:       row.VatID,
				CenaDetal:   f64(row.CenaDetal),
				CenaHurtowa: f64(row.CenaHurtowa),
				AktywnyWSI:  yn(row.AktywnyWSI),
				DoUsuniecia: yn(row.DoUsuniecia),
			})
			for _, mag := range row.Magazyny {
				stocks = append(stocks, expectedStock{
					TowarID:    row.TowarID,
					MagazynID:  mag.MagazynID,
					Stan:       f64(mag.Stan),
					Rezerwacja: f64(mag.Rezerwacja),
				})
			}
		}
	}
	return products, stocks
}

func applyExpectedImport(state *expectedImportState, importID uint, products []expectedProduct, stocks []expectedStock) {
	for _, product := range products {
		product.ImportID = importID
		state.Products[productKey{TowarID: product.TowarID, Kod: product.Kod}] = product
	}
	for _, stock := range stocks {
		key := stockKey{TowarID: stock.TowarID, MagazynID: stock.MagazynID}
		if prev, ok := state.Stocks[key]; ok {
			prevStan := prev.Stan
			stock.StanPrev = &prevStan
		}
		stock.ImportID = importID
		state.Stocks[key] = stock
	}
}

func assertExpectedImportState(t *testing.T, gdb *gorm.DB, expected expectedImportState, checkpoint int, filename string) {
	t.Helper()

	var products []db.StProduct
	if err := gdb.Find(&products).Error; err != nil {
		t.Fatal(err)
	}
	if len(products) != len(expected.Products) {
		t.Fatalf("checkpoint %d %s: products count got %d want %d", checkpoint, filename, len(products), len(expected.Products))
	}
	for _, got := range products {
		want, ok := expected.Products[productKey{TowarID: got.TowarID, Kod: got.Kod}]
		if !ok {
			t.Fatalf("checkpoint %d %s: unexpected product towar_id=%d kod=%q", checkpoint, filename, got.TowarID, got.Kod)
		}
		if got.ImportID != want.ImportID ||
			got.Nazwa != want.Nazwa ||
			got.VatID != want.VatID ||
			!sameTestFloat(got.CenaDetal, want.CenaDetal) ||
			!sameTestFloat(got.CenaHurtowa, want.CenaHurtowa) ||
			got.AktywnyWSI != want.AktywnyWSI ||
			got.DoUsuniecia != want.DoUsuniecia {
			t.Fatalf("checkpoint %d %s: product mismatch for towar_id=%d kod=%q got=%+v want=%+v", checkpoint, filename, got.TowarID, got.Kod, got, want)
		}
	}

	var stocks []db.StStock
	if err := gdb.Find(&stocks).Error; err != nil {
		t.Fatal(err)
	}
	if len(stocks) != len(expected.Stocks) {
		t.Fatalf("checkpoint %d %s: stocks count got %d want %d", checkpoint, filename, len(stocks), len(expected.Stocks))
	}
	for _, got := range stocks {
		want, ok := expected.Stocks[stockKey{TowarID: got.TowarID, MagazynID: got.MagazynID}]
		if !ok {
			t.Fatalf("checkpoint %d %s: unexpected stock towar_id=%d magazyn_id=%d", checkpoint, filename, got.TowarID, got.MagazynID)
		}
		if got.ImportID != want.ImportID ||
			!sameTestFloat(got.Stan, want.Stan) ||
			!sameTestFloat(got.Rezerwacja, want.Rezerwacja) ||
			!sameOptionalFloat(got.StanPrev, want.StanPrev) {
			t.Fatalf("checkpoint %d %s: stock mismatch for towar_id=%d magazyn_id=%d got=%+v want=%+v", checkpoint, filename, got.TowarID, got.MagazynID, got, want)
		}
	}
	assertNoDuplicateImportRows(t, gdb)
}

func assertPlannerUsesGrossStagingPrice(t *testing.T, gdb *gorm.DB, imp *Importer, importID uint) bool {
	t.Helper()

	var product db.StProduct
	err := gdb.
		Where("import_id = ? AND cena_detal > 0 AND kod <> ''", importID).
		Order("towar_id ASC").
		Take(&product).Error
	if err == gorm.ErrRecordNotFound {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}

	wooID := uint(900000 + importID)
	if err := gdb.Create(&db.WooProductCache{
		WooID:        wooID,
		TowarID:      &product.TowarID,
		Kod:          "TEST-GROSS",
		Ean:          product.Kod,
		Name:         product.Nazwa,
		PriceRegular: product.CenaDetal + 1,
		HurtPrice:    product.CenaHurtowa + 1,
		StockManaged: true,
		StockStatus:  "instock",
		Backorders:   "notify",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := imp.PlanWooTasks(importID); err != nil {
		t.Fatal(err)
	}

	var task db.WooTask
	if err := gdb.
		Where("import_id = ? AND woo_id = ? AND kind = ?", importID, wooID, db.WooTaskKindPriceUpdate).
		Take(&task).Error; err != nil {
		t.Fatal(err)
	}
	var payload db.WooPriceUpdatePayload
	if err := json.Unmarshal([]byte(task.PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if !sameTestFloat(payload.DesiredRegular, product.CenaDetal) {
		t.Fatalf("price.update should use gross staging cena_detal unchanged: got %v want %v", payload.DesiredRegular, product.CenaDetal)
	}
	if !sameTestFloat(payload.DesiredHurt, product.CenaHurtowa) {
		t.Fatalf("price.update should use gross staging cena_hurtowa unchanged: got %v want %v", payload.DesiredHurt, product.CenaHurtowa)
	}
	return true
}

func mustImportFile(t *testing.T, gdb *gorm.DB, filename string) db.ImportFile {
	t.Helper()

	var row db.ImportFile
	if err := gdb.Where("filename = ?", filename).Take(&row).Error; err != nil {
		t.Fatalf("import_files missing %s: %v", filename, err)
	}
	return row
}

func copyTestFile(t *testing.T, sourcePath, destPath string) {
	t.Helper()

	raw, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func sameTestFloat(a, b float64) bool {
	return math.Abs(a-b) < 0.000001
}

func sameOptionalFloat(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return sameTestFloat(*a, *b)
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
