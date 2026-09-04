package importer

import (
	"bufio"
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bartek5186/pcm2www/internal/db"
	"github.com/bartek5186/pcm2www/internal/integrations"
	"github.com/rs/zerolog"
	"golang.org/x/net/html/charset"
	"gorm.io/gorm"
)

const (
	realXMLFixtureEnv = "PCM2WWW_IMPORT_XML_FIXTURE_TESTS"
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

type stockKey struct {
	TowarID   int64
	MagazynID int64
}

type expectedProduct struct {
	ImportID         uint
	TowarID          int64
	Kod              string
	Nazwa            string
	Opis1            string
	VatID            int64
	KategoriaID      int64
	GrupaID          int64
	JmID             int64
	CenaDetal        float64
	CenaHurtowa      float64
	CenaNocna        float64
	CenaDodatkowa    float64
	CenaDetPrzedProm float64
	NajCena30Det     float64
	AktywnyWSI       bool
	DoUsuniecia      bool
	DataAktualizacji string
	FolderZdjec      string
	PlikZdjecia      string
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
	Products map[int64]expectedProduct
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
		Products: make(map[int64]expectedProduct),
		Stocks:   make(map[stockKey]expectedStock),
	}
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
		assertArchivedXML(t, watchDir, name)

		applyExpectedImport(&expected, importFile.ImportID, expectedProducts, expectedStocks)
		after := snapshotImportDB(t, gdb, importFile.ImportID)
		if after.CurrentProducts != int64(len(expectedProducts)) {
			t.Fatalf("%s current product rows got=%d want=%d", name, after.CurrentProducts, len(expectedProducts))
		}
		if after.CurrentStocks != int64(len(expectedStocks)) {
			t.Fatalf("%s current stock rows got=%d want=%d", name, after.CurrentStocks, len(expectedStocks))
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

		// Każdy XML jest osobną klatką stanu. Porównanie całej bazy po każdym
		// kroku wykrywa również błędy przejściowe, które zniknęłyby do końca serii.
		assertExpectedImportState(t, gdb, expected, idx+1, name)

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
	lastID, already, err := imp.registerFile(filepath.Join(watchDir, "parsed", lastName), lastName)
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
	assertFileExists(t, brokenPath)

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

	goodName := "exp_wyk_valid_after_broken.xml"
	goodXML := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<root><transmisja_id>valid-after-broken-1</transmisja_id><towary><towar>
<towar_id>2</towar_id><kod>5901234567890</kod><nazwa>Valid product</nazwa><cena_detal>12.34</cena_detal>
<magazyny><magazyn><magazyn_id>1</magazyn_id><stan_magazynu>3</stan_magazynu></magazyn></magazyny>
</towar></towary></root>`)
	if err := os.WriteFile(filepath.Join(watchDir, goodName), goodXML, 0o600); err != nil {
		t.Fatal(err)
	}
	imp.scanOnce(watchDir)

	goodImport := mustImportFile(t, gdb, goodName)
	if goodImport.Status != 1 {
		t.Fatalf("valid XML after broken file should import successfully, got status=%d error=%q", goodImport.Status, goodImport.LastError)
	}
	assertArchivedXML(t, watchDir, goodName)
	mustCount(t, gdb.Model(&db.StProduct{}), &products)
	mustCount(t, gdb.Model(&db.StStock{}), &stocks)
	if products == 0 || stocks == 0 {
		t.Fatalf("valid XML after broken file did not seed staging: products=%d stocks=%d", products, stocks)
	}

	if err := os.WriteFile(filepath.Join(watchDir, goodName), goodXML, 0o600); err != nil {
		t.Fatal(err)
	}
	imp.scanOnce(watchDir)
	assertFileMissing(t, filepath.Join(watchDir, goodName))

	duplicateArchived, err := filepath.Glob(filepath.Join(watchDir, "parsed", strings.TrimSuffix(goodName, filepath.Ext(goodName))+".import_*"+filepath.Ext(goodName)))
	if err != nil {
		t.Fatal(err)
	}
	if len(duplicateArchived) == 0 {
		t.Fatalf("expected duplicate DONE XML to be archived with import suffix")
	}
}

func TestImportInvalidNumericValueDoesNotDisableProducts(t *testing.T) {
	gdb := newImporterTestDB(t)
	watchDir := t.TempDir()
	imp := &Importer{log: zerolog.Nop(), db: gdb}

	name := "exp_wyk_9999_20260615121000.xml"
	path := filepath.Join(watchDir, name)
	raw := `<?xml version="1.0" encoding="UTF-8"?>
<root><transmisja_id>invalid-number-1</transmisja_id><towary><towar>
<towar_id>1</towar_id><kod>5901234567890</kod><nazwa>Invalid price</nazwa>
<cena_detal>not-a-number</cena_detal><cena_hurtowa>10</cena_hurtowa>
</towar></towary></root>`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	imp.scanOnce(watchDir)

	importFile := mustImportFile(t, gdb, name)
	if importFile.Status != 2 || !strings.Contains(importFile.LastError, "invalid cena_detal") {
		t.Fatalf("invalid number should fail import explicitly, got status=%d error=%q", importFile.Status, importFile.LastError)
	}
	var products int64
	mustCount(t, gdb.Model(&db.StProduct{}), &products)
	if products != 0 {
		t.Fatalf("invalid numeric import left %d staging products", products)
	}
}

func TestRegisterFileAllowsReusedFilenameForNewExport(t *testing.T) {
	gdb := newImporterTestDB(t)
	imp := &Importer{log: zerolog.Nop(), db: gdb}
	path := filepath.Join(t.TempDir(), "exp_wyk_current.xml")

	first := `<?xml version="1.0"?><root><transmisja_id>tx-one</transmisja_id></root>`
	if err := os.WriteFile(path, []byte(first), 0o600); err != nil {
		t.Fatal(err)
	}
	firstID, already, err := imp.registerFile(path, filepath.Base(path))
	if err != nil || already {
		t.Fatalf("first registration: id=%d already=%v err=%v", firstID, already, err)
	}

	second := `<?xml version="1.0"?><root><transmisja_id>tx-two</transmisja_id></root>`
	if err := os.WriteFile(path, []byte(second), 0o600); err != nil {
		t.Fatal(err)
	}
	secondID, already, err := imp.registerFile(path, filepath.Base(path))
	if err != nil || already {
		t.Fatalf("second registration with reused name: id=%d already=%v err=%v", secondID, already, err)
	}
	if firstID == secondID {
		t.Fatalf("different exports with the same filename received one import id: %d", firstID)
	}
}

func TestImportEANChangeUpdatesSamePCMProduct(t *testing.T) {
	gdb := newImporterTestDB(t)
	watchDir := t.TempDir()
	imp := &Importer{log: zerolog.Nop(), db: gdb}
	write := func(name, transmission, ean string) {
		raw := `<?xml version="1.0"?><root><transmisja_id>` + transmission + `</transmisja_id><towary><towar>` +
			`<towar_id>44</towar_id><kod>` + ean + `</kod><nazwa>Changed EAN</nazwa>` +
			`</towar></towary></root>`
		if err := os.WriteFile(filepath.Join(watchDir, name), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		imp.scanOnce(watchDir)
	}
	write("exp_wyk_ean_1.xml", "ean-change-1", "5900000000044")
	write("exp_wyk_ean_2.xml", "ean-change-2", "5900000000099")

	var products []db.StProduct
	if err := gdb.Where("towar_id = ?", 44).Find(&products).Error; err != nil {
		t.Fatal(err)
	}
	if len(products) != 1 || products[0].Kod != "5900000000099" {
		t.Fatalf("EAN change created a stale PCM row: %+v", products)
	}
}

func TestRegisterFileDeduplicatesByNonEmptyTransmissionID(t *testing.T) {
	gdb := newImporterTestDB(t)
	imp := &Importer{log: zerolog.Nop(), db: gdb}
	path := filepath.Join(t.TempDir(), "exp_wyk_transmission.xml")

	first := `<?xml version="1.0"?><root><transmisja_id>same-tx</transmisja_id><value>one</value></root>`
	if err := os.WriteFile(path, []byte(first), 0o600); err != nil {
		t.Fatal(err)
	}
	firstID, already, err := imp.registerFile(path, filepath.Base(path))
	if err != nil || already {
		t.Fatalf("first registration: id=%d already=%v err=%v", firstID, already, err)
	}

	second := `<?xml version="1.0"?><root><transmisja_id>same-tx</transmisja_id><value>changed</value></root>`
	if err := os.WriteFile(path, []byte(second), 0o600); err != nil {
		t.Fatal(err)
	}
	secondID, already, err := imp.registerFile(path, filepath.Base(path))
	if err != nil || !already || secondID != firstID {
		t.Fatalf("same transmission should deduplicate: first=%d second=%d already=%v err=%v", firstID, secondID, already, err)
	}
}

func TestFindRegisteredFileRejectsConflictingIdentity(t *testing.T) {
	gdb := newImporterTestDB(t)
	rows := []db.ImportFile{
		{Filename: "first.xml", SHA256: "hash-one", TransmisjaID: "tx-one"},
		{Filename: "second.xml", SHA256: "hash-two", TransmisjaID: "tx-two"},
	}
	if err := gdb.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}

	_, found, err := findRegisteredFile(gdb, "hash-one", "tx-two")
	if err == nil || found || !strings.Contains(err.Error(), "conflicting import identity") {
		t.Fatalf("expected explicit identity conflict, found=%v err=%v", found, err)
	}
}

func TestImporterIgnoresZipFiles(t *testing.T) {
	gdb := newImporterTestDB(t)
	watchDir := t.TempDir()
	imp := &Importer{log: zerolog.Nop(), db: gdb}
	path := filepath.Join(watchDir, "exp_wyk_not_supported.zip")
	if err := os.WriteFile(path, []byte("not an XML import"), 0o600); err != nil {
		t.Fatal(err)
	}

	imp.scanOnce(watchDir)

	var imports int64
	mustCount(t, gdb.Model(&db.ImportFile{}), &imports)
	if imports != 0 {
		t.Fatalf("ZIP file created %d import records", imports)
	}
	assertFileExists(t, path)
}

func TestImporterWaitsForStableXMLFile(t *testing.T) {
	gdb := newImporterTestDB(t)
	watchDir := t.TempDir()
	imp := &Importer{log: zerolog.Nop(), db: gdb, cfg: Config{StabilitySeconds: 2}}
	name := "exp_wyk_stable_20260904120000.xml"
	path := filepath.Join(watchDir, name)
	raw := `<?xml version="1.0"?><root><transmisja_id>stable-1</transmisja_id><towary><towar><towar_id>1</towar_id><kod>5901</kod></towar></towary></root>`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	imp.scanOnce(watchDir)
	var imports int64
	mustCount(t, gdb.Model(&db.ImportFile{}), &imports)
	if imports != 0 {
		t.Fatalf("newly observed file should not be imported, got %d records", imports)
	}
	observation := imp.observedFiles[path]
	observation.firstSeen = time.Now().Add(-3 * time.Second)
	imp.observedFiles[path] = observation
	imp.scanOnce(watchDir)

	row := mustImportFile(t, gdb, name)
	if row.Status != 1 {
		t.Fatalf("stable file was not imported: %+v", row)
	}
}

func TestImporterWaitsForWooCacheReadinessBeforeLinkAndPlan(t *testing.T) {
	gdb := newImporterTestDB(t)
	watchDir := t.TempDir()
	towarID := int64(81)
	wooID := uint(91)
	if err := gdb.Create(&db.WooProductCache{WooID: wooID, Ean: "5900000000081", StockManaged: true}).Error; err != nil {
		t.Fatal(err)
	}
	raw := `<?xml version="1.0"?><root><transmisja_id>ready-1</transmisja_id><towary><towar>` +
		`<towar_id>81</towar_id><kod>5900000000081</kod><nazwa>Ready product</nazwa><cena_detal>10</cena_detal>` +
		`<magazyny><magazyn><magazyn_id>1</magazyn_id><stan_magazynu>2</stan_magazynu></magazyn></magazyny>` +
		`</towar></towary></root>`
	if err := os.WriteFile(filepath.Join(watchDir, "exp_wyk_ready.xml"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	runtime := integrations.NewRuntime(gdb, true)
	ctx, cancel := context.WithCancel(integrations.WithRuntime(context.Background(), runtime))
	defer cancel()
	imp := &Importer{log: zerolog.Nop(), cfg: Config{WatchDir: watchDir, PollSec: 60}}
	done := make(chan error, 1)
	go func() { done <- imp.Start(ctx) }()

	waitForTest(t, time.Second, func() bool {
		var count int64
		return gdb.Model(&db.StProduct{}).Where("towar_id = ?", towarID).Count(&count).Error == nil && count == 1
	})
	var before db.WooProductCache
	if err := gdb.Where("woo_id = ?", wooID).Take(&before).Error; err != nil {
		t.Fatal(err)
	}
	if before.TowarID != nil {
		t.Fatal("importer linked before Woo cache readiness signal")
	}

	runtime.MarkWooCacheReady()
	waitForTest(t, time.Second, func() bool {
		var cache db.WooProductCache
		return gdb.Where("woo_id = ?", wooID).Take(&cache).Error == nil && cache.TowarID != nil && *cache.TowarID == towarID
	})
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("importer did not stop")
	}
}

func waitForTest(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}

func TestImportRejectsDuplicateNaturalKeys(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantError string
	}{
		{
			name: "product",
			body: `<towar><towar_id>1</towar_id><kod>5901</kod></towar>
<towar><towar_id>1</towar_id><kod>5901</kod></towar>`,
			wantError: "duplicate product in XML",
		},
		{
			name: "stock",
			body: `<towar><towar_id>1</towar_id><kod>5901</kod><magazyny>
<magazyn><magazyn_id>1</magazyn_id><stan_magazynu>2</stan_magazynu></magazyn>
<magazyn><magazyn_id>1</magazyn_id><stan_magazynu>3</stan_magazynu></magazyn>
</magazyny></towar>`,
			wantError: "duplicate stock in XML",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gdb := newImporterTestDB(t)
			watchDir := t.TempDir()
			imp := &Importer{log: zerolog.Nop(), db: gdb}
			name := "exp_wyk_duplicate_" + tt.name + ".xml"
			path := filepath.Join(watchDir, name)
			raw := `<?xml version="1.0"?><root><transmisja_id>duplicate-` + tt.name + `</transmisja_id><towary>` + tt.body + `</towary></root>`
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}

			imp.scanOnce(watchDir)

			importFile := mustImportFile(t, gdb, name)
			if importFile.Status != 2 || !strings.Contains(importFile.LastError, tt.wantError) {
				t.Fatalf("duplicate should fail atomically, got status=%d error=%q", importFile.Status, importFile.LastError)
			}
			var products, stocks int64
			mustCount(t, gdb.Model(&db.StProduct{}), &products)
			mustCount(t, gdb.Model(&db.StStock{}), &stocks)
			if products != 0 || stocks != 0 {
				t.Fatalf("duplicate import left partial rows: products=%d stocks=%d", products, stocks)
			}
		})
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
			kategoriaID, err := i64(row.KategoriaID)
			if err != nil {
				t.Fatalf("parse fixture kategoria_id for towar_id=%d: %v", row.TowarID, err)
			}
			grupaID, err := i64(row.GrupaID)
			if err != nil {
				t.Fatalf("parse fixture asortyment_id for towar_id=%d: %v", row.TowarID, err)
			}
			products = append(products, expectedProduct{
				TowarID:          row.TowarID,
				Kod:              kod,
				Nazwa:            strings.TrimSpace(row.Nazwa),
				Opis1:            row.Opis1,
				VatID:            row.VatID,
				KategoriaID:      kategoriaID,
				GrupaID:          grupaID,
				JmID:             row.JmID,
				CenaDetal:        mustF64(t, row.CenaDetal),
				CenaHurtowa:      mustF64(t, row.CenaHurtowa),
				CenaNocna:        mustF64(t, row.CenaNocna),
				CenaDodatkowa:    mustF64(t, row.CenaDodatkowa),
				CenaDetPrzedProm: mustF64(t, row.CenaDetPrzed),
				NajCena30Det:     mustF64(t, row.NajCena30Det),
				AktywnyWSI:       yn(row.AktywnyWSI),
				DoUsuniecia:      yn(row.DoUsuniecia),
				DataAktualizacji: row.DataAktualizacji,
				FolderZdjec:      row.FolderZdjec,
				PlikZdjecia:      row.PlikZdjecia,
			})
			for _, mag := range row.Magazyny {
				stocks = append(stocks, expectedStock{
					TowarID:    row.TowarID,
					MagazynID:  mag.MagazynID,
					Stan:       mustF64(t, mag.Stan),
					Rezerwacja: mustF64(t, mag.Rezerwacja),
				})
			}
		}
	}
	return products, stocks
}

func mustF64(t *testing.T, raw string) float64 {
	t.Helper()
	value, err := f64(raw)
	if err != nil {
		t.Fatalf("parse fixture number %q: %v", raw, err)
	}
	return value
}

func applyExpectedImport(state *expectedImportState, importID uint, products []expectedProduct, stocks []expectedStock) {
	for _, product := range products {
		product.ImportID = importID
		state.Products[product.TowarID] = product
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
		want, ok := expected.Products[got.TowarID]
		if !ok {
			t.Fatalf("checkpoint %d %s: unexpected product towar_id=%d kod=%q", checkpoint, filename, got.TowarID, got.Kod)
		}
		if got.ImportID != want.ImportID ||
			got.Kod != want.Kod ||
			got.Nazwa != want.Nazwa ||
			got.Opis1 != want.Opis1 ||
			got.VatID != want.VatID ||
			got.KategoriaID != want.KategoriaID ||
			got.GrupaID != want.GrupaID ||
			got.JmID != want.JmID ||
			!sameTestFloat(got.CenaDetal, want.CenaDetal) ||
			!sameTestFloat(got.CenaHurtowa, want.CenaHurtowa) ||
			!sameTestFloat(got.CenaNocna, want.CenaNocna) ||
			!sameTestFloat(got.CenaDodatkowa, want.CenaDodatkowa) ||
			!sameTestFloat(got.CenaDetPrzedProm, want.CenaDetPrzedProm) ||
			!sameTestFloat(got.NajCena30Det, want.NajCena30Det) ||
			got.AktywnyWSI != want.AktywnyWSI ||
			got.DoUsuniecia != want.DoUsuniecia ||
			got.DataAktualizacji != want.DataAktualizacji ||
			got.FolderZdjec != want.FolderZdjec ||
			got.PlikZdjecia != want.PlikZdjecia {
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

func assertArchivedXML(t *testing.T, watchDir, name string) {
	t.Helper()
	assertFileMissing(t, filepath.Join(watchDir, name))
	assertFileExists(t, filepath.Join(watchDir, "parsed", name))
}

func assertFileExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file %s to exist: %v", path, err)
	}
}

func assertFileMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("expected file %s to be moved away", path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", path, err)
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
	SELECT towar_id
	FROM st_products
	GROUP BY towar_id
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
