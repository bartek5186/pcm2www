package db

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Migrate tworzy lub aktualizuje schemat bez usuwania poprawnych danych.
func (h *Handle) Migrate() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	gdb := h.DB.WithContext(ctx)

	migrationPending := true
	if gdb.Migrator().HasTable(&SchemaMigration{}) {
		var applied int64
		if err := gdb.Model(&SchemaMigration{}).Where("version = ?", 3).Count(&applied).Error; err != nil {
			return fmt.Errorf("check migration version: %w", err)
		}
		migrationPending = applied == 0
	}
	if migrationPending {
		if err := h.backupSQLiteBeforeMigration(gdb); err != nil {
			return err
		}
	}
	if err := gdb.AutoMigrate(&SchemaMigration{}); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	// Starsze wersje traktowały nazwę pliku i transmisja_id jako globalnie
	// unikalne. Nazwa może zostać ponownie użyta dla innej zawartości, a puste
	// transmisja_id nie jest przenośnym kluczem unikalnym między silnikami DB.
	if gdb.Migrator().HasTable(&ImportFile{}) {
		for _, index := range []string{"idx_import_files_filename", "idx_import_files_transmisja_id"} {
			if gdb.Migrator().HasIndex(&ImportFile{}, index) {
				if err := gdb.Migrator().DropIndex(&ImportFile{}, index); err != nil {
					return fmt.Errorf("drop legacy index %s: %w", index, err)
				}
			}
		}
	}

	// Stara baza mogła zawierać powtórzone diagnostyki sprzed utworzenia
	// indeksu złożonego. Usuń wyłącznie nadmiarowe kopie, zachowując najnowszą.
	if gdb.Migrator().HasTable(&LinkIssue{}) {
		if err := deduplicateLinkIssues(gdb); err != nil {
			return fmt.Errorf("deduplicate link_issues: %w", err)
		}
	}
	if gdb.Migrator().HasTable(&StProduct{}) {
		if gdb.Migrator().HasIndex(&StProduct{}, "uniq_towar_kod") {
			if err := gdb.Migrator().DropIndex(&StProduct{}, "uniq_towar_kod"); err != nil {
				return fmt.Errorf("drop legacy product identity index: %w", err)
			}
		}
		if err := deduplicateStProducts(gdb); err != nil {
			return fmt.Errorf("deduplicate st_products by towar_id: %w", err)
		}
	}

	if err := gdb.AutoMigrate(
		&ImportFile{},
		&StProduct{},
		&StStock{},
		&WooProductCache{},
		&WooTask{},
		&KV{},
		&LinkIssue{},
		&SchemaMigration{},
	); err != nil {
		return fmt.Errorf("AutoMigrate error: %w", err)
	}

	if !gdb.Migrator().HasIndex(&LinkIssue{}, "uniq_issue_key") {
		if err := gdb.Migrator().CreateIndex(&LinkIssue{}, "uniq_issue_key"); err != nil {
			return fmt.Errorf("create index uniq_issue_key: %w", err)
		}
	}
	if err := gdb.Clauses(clause.OnConflict{DoNothing: true}).Create(&SchemaMigration{
		Version: 2, Name: "stable product identity and diagnostics indexes", AppliedAt: time.Now().UTC(),
	}).Error; err != nil {
		return fmt.Errorf("record schema migration: %w", err)
	}

	if err := gdb.Clauses(clause.OnConflict{DoNothing: true}).Create(&SchemaMigration{
		Version: 3, Name: "task revisions, reservation history and durable planning", AppliedAt: time.Now().UTC(),
	}).Error; err != nil {
		return fmt.Errorf("record schema migration: %w", err)
	}

	return nil
}

func (h *Handle) backupSQLiteBeforeMigration(gdb *gorm.DB) error {
	if h.Driver != "sqlite" || strings.TrimSpace(h.Path) == "" {
		return nil
	}
	info, err := os.Stat(h.Path)
	if os.IsNotExist(err) || (err == nil && info.Size() == 0) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect SQLite before migration: %w", err)
	}
	backup := h.Path + ".backup-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	if err := gdb.Exec("VACUUM INTO ?", backup).Error; err != nil {
		return fmt.Errorf("backup SQLite before migration: %w", err)
	}
	backups, _ := filepath.Glob(h.Path + ".backup-*")
	sort.Strings(backups)
	for len(backups) > 5 {
		if err := os.Remove(backups[0]); err != nil {
			return fmt.Errorf("remove old SQLite migration backup: %w", err)
		}
		backups = backups[1:]
	}
	return nil
}

func deduplicateStProducts(gdb *gorm.DB) error {
	var rows []StProduct
	if err := gdb.Order("updated_at DESC").Order("id DESC").Find(&rows).Error; err != nil {
		return err
	}
	seen := make(map[int64]struct{}, len(rows))
	duplicateIDs := make([]uint, 0)
	for _, row := range rows {
		if _, exists := seen[row.TowarID]; exists {
			duplicateIDs = append(duplicateIDs, row.ID)
			continue
		}
		seen[row.TowarID] = struct{}{}
	}
	for len(duplicateIDs) > 0 {
		n := min(len(duplicateIDs), 500)
		if err := gdb.Where("id IN ?", duplicateIDs[:n]).Delete(&StProduct{}).Error; err != nil {
			return err
		}
		duplicateIDs = duplicateIDs[n:]
	}
	return nil
}

func deduplicateLinkIssues(gdb *gorm.DB) error {
	var rows []LinkIssue
	if err := gdb.Order("updated_at DESC").Order("id DESC").Find(&rows).Error; err != nil {
		return err
	}

	type issueKey struct {
		TowarID int64
		Reason  string
		Kod     string
	}
	seen := make(map[issueKey]struct{}, len(rows))
	duplicateIDs := make([]uint, 0)
	for _, row := range rows {
		key := issueKey{TowarID: row.TowarID, Reason: row.Reason, Kod: row.Kod}
		if _, ok := seen[key]; ok {
			duplicateIDs = append(duplicateIDs, row.ID)
			continue
		}
		seen[key] = struct{}{}
	}

	for len(duplicateIDs) > 0 {
		n := min(len(duplicateIDs), 500)
		if err := gdb.Where("id IN ?", duplicateIDs[:n]).Delete(&LinkIssue{}).Error; err != nil {
			return err
		}
		duplicateIDs = duplicateIDs[n:]
	}
	return nil
}
