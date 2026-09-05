package problems

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/bartek5186/pcm2www/internal/db"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := gdb.AutoMigrate(&db.StProduct{}, &db.WooProductCache{}, &db.WooTask{}, &db.ImportFile{}); err != nil {
		t.Fatal(err)
	}
	return gdb
}

func TestCurrentEANProblemsAndProductLinks(t *testing.T) {
	gdb := testDB(t)
	for _, rows := range []any{
		&[]db.StProduct{
			{TowarID: 1, Kod: "", Nazwa: "Brak kodu"}, {TowarID: 2, Kod: "002", Nazwa: "Brak w sklepie"},
			{TowarID: 3, Kod: "003"}, {TowarID: 4, Kod: "0-03"}, // duplicate PCM
			{TowarID: 5, Kod: "005"}, {TowarID: 6, Kod: "006"}, // duplicate Woo and valid match
		},
		&[]db.WooProductCache{
			{WooID: 10, Ean: "", Name: "Żółty produkt"}, {WooID: 11, Ean: "0011"},
			{WooID: 12, Ean: "003"}, {WooID: 13, Ean: "005"}, {WooID: 14, Ean: "005"}, {WooID: 15, Ean: "0-06"},
		},
	} {
		if err := gdb.Create(rows).Error; err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := Load(context.Background(), gdb, "https://user:secret@example.org/sklep/?consumer_key=secret#fragment")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"missing_ean_src": 1, "missing_in_shop_by_ean": 1, "duplicate_ean_source": 2, "missing_ean_shop": 1, "missing_in_magazine_by_ean": 1, "duplicate_ean_shop": 2}
	for _, r := range snapshot.Rows {
		want[r.Code]--
		if r.WooID == 15 {
			t.Fatal("normalised unique EAN incorrectly reported")
		}
		if r.WooID != 0 && (!strings.Contains(r.EditURL, "/sklep/wp-admin/post.php?") || !strings.Contains(r.ProductURL, "post_type=product")) {
			t.Fatalf("invalid links: %+v", r)
		}
		if strings.Contains(r.EditURL+r.ProductURL, "secret") {
			t.Fatal("credentials leaked into links")
		}
	}
	for code, count := range want {
		if count != 0 {
			t.Fatalf("wrong count for %s: delta=%d", code, count)
		}
	}
	// A current cache change is reflected without running the linker.
	if err := gdb.Model(&db.WooProductCache{}).Where("woo_id = ?", 10).Update("ean", "002").Error; err != nil {
		t.Fatal(err)
	}
	next, err := Load(context.Background(), gdb, "https://example.org")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range next.Rows {
		if r.Code == "missing_in_shop_by_ean" || r.Code == "missing_ean_shop" {
			t.Fatalf("resolved issue still reported: %+v", r)
		}
	}
}

func TestReportSuppressesHistoricalTaskErrors(t *testing.T) {
	gdb := testDB(t)
	wooID := uint(10)
	rows := []db.WooTask{
		{TaskKey: "old", WooID: &wooID, Kind: "price.update", Revision: 1, Status: "error", LastError: "old price error"},
		{TaskKey: "new", WooID: &wooID, Kind: "price.update", Revision: 2, Status: "done"},
		{TaskKey: "stock", WooID: &wooID, Kind: "stock.update", Revision: 1, Status: "error", LastError: "stock verification mismatch", ImportID: 2},
		{TaskKey: "retry", WooID: &wooID, Kind: "availability.update", Revision: 1, Status: "pending", LastError: "HTTP 503"},
		{TaskKey: "superseded", Kind: "price.update", Status: "superseded", LastError: "obsolete"},
		{TaskKey: "old-ean", WooID: &wooID, Kind: "ean.update", Revision: 1, Status: "error", LastError: "obsolete EAN error"},
		{TaskKey: "cancelled-ean", WooID: &wooID, Kind: "ean.update", Revision: 2, Status: "superseded"},
	}
	if err := gdb.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&db.ImportFile{ImportID: 2, Filename: "broken.xml", Status: 2, LastError: "invalid XML"}).Error; err != nil {
		t.Fatal(err)
	}
	snapshot, err := Load(context.Background(), gdb, "https://example.org")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Rows) != 3 {
		t.Fatalf("wrong unresolved report: %+v", snapshot.Rows)
	}
	for _, r := range snapshot.Rows {
		if strings.Contains(r.Details, "old") || strings.Contains(r.Details, "obsolete") {
			t.Fatalf("history leaked into report: %+v", r)
		}
		if r.Code == "task_error" && (r.TaskID != rows[2].TaskID || r.File != "broken.xml" || r.EditURL == "") {
			t.Fatalf("task lacks context: %+v", r)
		}
	}
	filtered := Filter(snapshot.Rows, "Synchronizacja", "stock verification")
	if len(filtered) != 1 || filtered[0].Code != "task_error" {
		t.Fatalf("incorrect filter: %+v", filtered)
	}
}

func TestCSVExcelEncodingEscapingAndWriterErrors(t *testing.T) {
	var buf bytes.Buffer
	at := time.Date(2026, 9, 5, 18, 0, 0, 0, time.UTC)
	rows := []Row{{Category: "EAN", PCMName: "Żółty; produkt", PCMCode: "0012345678901", Details: "=HYPERLINK(\"bad\")", ProductURL: "https://example.org/?p=1"}}
	if err := WriteCSV(&buf, at, rows); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(buf.Bytes(), []byte{0xef, 0xbb, 0xbf}) {
		t.Fatal("missing UTF-8 BOM")
	}
	r := csv.NewReader(strings.NewReader(strings.TrimPrefix(buf.String(), "\ufeff")))
	r.Comma = ';'
	records, err := r.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[1][4] != "Żółty; produkt" || records[1][6] != "0012345678901" || records[1][16] != "'=HYPERLINK(\"bad\")" {
		t.Fatalf("CSV changed data or allowed formula: %+v", records)
	}
	if err := WriteCSV(failingWriter{}, at, rows); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write failure not returned: %v", err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
