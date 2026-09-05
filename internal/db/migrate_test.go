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
	closeMigrationTestDB(t, h.DB)
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
	closeMigrationTestDB(t, gdb)
	return &Handle{DB: gdb, Driver: "sqlite"}
}

func closeMigrationTestDB(t *testing.T, gdb *gorm.DB) {
	t.Helper()
	sqlDB, err := gdb.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close migration test DB: %v", err)
		}
	})
}

func TestMigrationV3PreservesV2DataAndBacksUpOnlyOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.db")
	h, err := OpenWithConfig(filepath.Dir(path), OpenConfig{Driver: "sqlite", Path: path})
	if err != nil {
		t.Fatal(err)
	}
	closeMigrationTestDB(t, h.DB)
	if err := h.Migrate(); err != nil {
		t.Fatal(err)
	}
	// Reconstruct the preceding schema rather than leaving new columns in place.
	for _, col := range []struct {
		model any
		name  string
	}{
		{&ImportFile{}, "PlanningPending"}, {&StStock{}, "RezerwacjaPrev"}, {&WooTask{}, "Revision"},
	} {
		if err := h.DB.Migrator().DropColumn(col.model, col.name); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.DB.Where("version = ?", 3).Delete(&SchemaMigration{}).Error; err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"INSERT INTO import_files(import_id,filename,sha256,status) VALUES(1,'legacy.xml','legacy-sha',1)",
		"INSERT INTO st_stocks(towar_id,magazyn_id,stan,stan_prev,rezerwacja,import_id) VALUES(42,1,10,10,3,1)",
		"INSERT INTO woo_tasks(task_id,task_key,woo_id,kind,status,payload_json) VALUES(1,'legacy-price',900,'price.update','pending','{}')",
	} {
		if err := h.DB.Exec(query).Error; err != nil {
			t.Fatal(err)
		}
	}
	before, _ := filepath.Glob(path + ".backup-*")
	if err := h.Migrate(); err != nil {
		t.Fatal(err)
	}
	after, _ := filepath.Glob(path + ".backup-*")
	if len(after) != len(before)+1 {
		t.Fatalf("v3 should make a backup: before=%v after=%v", before, after)
	}
	var stock StStock
	if err := h.DB.First(&stock).Error; err != nil {
		t.Fatal(err)
	}
	if stock.Stan != 10 || stock.StanPrev == nil || *stock.StanPrev != 10 || stock.Rezerwacja != 3 || stock.RezerwacjaPrev != nil {
		t.Fatalf("legacy stock changed or history was fabricated: %+v", stock)
	}
	var task WooTask
	if err := h.DB.First(&task, "task_id = ?", 1).Error; err != nil {
		t.Fatal(err)
	}
	if task.Status != "pending" || task.Revision != 0 || task.PayloadJSON != "{}" {
		t.Fatalf("legacy task changed: %+v", task)
	}
	var file ImportFile
	if err := h.DB.First(&file, "import_id = ?", 1).Error; err != nil {
		t.Fatal(err)
	}
	if file.Status != 1 || file.PlanningPending {
		t.Fatalf("legacy import changed: %+v", file)
	}
	if err := h.Migrate(); err != nil {
		t.Fatal(err)
	}
	again, _ := filepath.Glob(path + ".backup-*")
	if len(again) != len(after) {
		t.Fatal("repeat migration created another backup")
	}
}
