package db

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func TestMigrateIsIdempotentAndPreservesDiagnostics(t *testing.T) {
	h := newMigrationTestHandle(t)
	if err := h.Migrate(); err != nil {
		t.Fatal(err)
	}

	issue := LinkIssue{TowarID: 10, Reason: "missing_ean_src", Kod: "ABC", Details: "keep me"}
	if err := h.DB.Create(&issue).Error; err != nil {
		t.Fatal(err)
	}
	if err := h.Migrate(); err != nil {
		t.Fatal(err)
	}

	var got LinkIssue
	if err := h.DB.Where("id = ?", issue.ID).Take(&got).Error; err != nil {
		t.Fatal(err)
	}
	if got.Details != "keep me" {
		t.Fatalf("migration changed valid diagnostic: %+v", got)
	}
}

func TestMigrateBacksUpSQLiteAndDeduplicatesLegacyProductIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	h, err := OpenWithConfig(filepath.Dir(path), OpenConfig{Driver: "sqlite", Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.DB.Exec(`CREATE TABLE st_products (
		id integer PRIMARY KEY AUTOINCREMENT,
		towar_id integer NOT NULL,
		kod text,
		nazwa text,
		updated_at datetime
	)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := h.DB.Exec("CREATE UNIQUE INDEX uniq_towar_kod ON st_products(towar_id, kod)").Error; err != nil {
		t.Fatal(err)
	}
	if err := h.DB.Exec("INSERT INTO st_products(towar_id,kod,nazwa,updated_at) VALUES (1,'old','old','2026-01-01'),(1,'new','new','2026-02-01')").Error; err != nil {
		t.Fatal(err)
	}
	if err := h.Migrate(); err != nil {
		t.Fatal(err)
	}
	var products []StProduct
	if err := h.DB.Find(&products).Error; err != nil {
		t.Fatal(err)
	}
	if len(products) != 1 || products[0].Kod != "new" {
		t.Fatalf("legacy products not deterministically deduplicated: %+v", products)
	}
	backups, err := filepath.Glob(path + ".backup-*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("expected one migration backup, got %v err=%v", backups, err)
	}
	var migration SchemaMigration
	if err := h.DB.Where("version = ?", 2).Take(&migration).Error; err != nil {
		t.Fatal(err)
	}
}

func TestMigrateDeduplicatesOnlyLegacyLinkIssues(t *testing.T) {
	h := newMigrationTestHandle(t)
	if err := h.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := h.DB.Migrator().DropIndex(&LinkIssue{}, "uniq_issue_key"); err != nil {
		t.Fatal(err)
	}

	older := LinkIssue{TowarID: 10, Reason: "duplicate_ean_source", Kod: "590", Details: "old", UpdatedAt: time.Now().Add(-time.Hour)}
	newer := LinkIssue{TowarID: 10, Reason: "duplicate_ean_source", Kod: "590", Details: "new", UpdatedAt: time.Now()}
	other := LinkIssue{TowarID: 11, Reason: "missing_ean_src", Kod: "", Details: "other"}
	if err := h.DB.Create([]LinkIssue{older, newer, other}).Error; err != nil {
		t.Fatal(err)
	}

	if err := h.Migrate(); err != nil {
		t.Fatal(err)
	}
	var rows []LinkIssue
	if err := h.DB.Order("towar_id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Details != "new" || rows[1].Details != "other" {
		t.Fatalf("unexpected diagnostics after deduplication: %+v", rows)
	}
	if !h.DB.Migrator().HasIndex(&LinkIssue{}, "uniq_issue_key") {
		t.Fatal("composite diagnostic index was not restored")
	}
}

func TestMigrateRemovesLegacyFileIdentityIndexes(t *testing.T) {
	h := newMigrationTestHandle(t)
	if err := h.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := h.DB.Exec("CREATE UNIQUE INDEX idx_import_files_filename ON import_files(filename)").Error; err != nil {
		t.Fatal(err)
	}
	if err := h.DB.Exec("CREATE UNIQUE INDEX idx_import_files_transmisja_id ON import_files(transmisja_id)").Error; err != nil {
		t.Fatal(err)
	}

	if err := h.Migrate(); err != nil {
		t.Fatal(err)
	}
	rows := []ImportFile{
		{Filename: "exp_wyk_same.xml", SHA256: "hash-one", TransmisjaID: "", Status: 1},
		{Filename: "exp_wyk_same.xml", SHA256: "hash-two", TransmisjaID: "", Status: 1},
	}
	if err := h.DB.Create(&rows).Error; err != nil {
		t.Fatalf("filename and empty transmission id must not be unique identities: %v", err)
	}
}

func newMigrationTestHandle(t *testing.T) *Handle {
	t.Helper()
	dsn := fmt.Sprintf("file:migrate-%s?mode=memory&cache=shared", t.Name())
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	return &Handle{DB: gdb, Driver: "sqlite"}
}
