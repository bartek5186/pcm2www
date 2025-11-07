package db

import (
	"fmt"
)

// Migrate tworzy/aktualizuje schemat bazy.
// Kolejność:
//  1. jeśli tabela istnieje -> hard purge + drop index
//  2. AutoMigrate
//  3. upewnij się, że indeks istnieje (jeśli NIE używasz tagów unikalności w modelu)
func (h *Handle) Migrate() error {
	gdb := h.DB

	// 1) Jeśli tabela istnieje: twardo wyczyść i zdejmij indeks, żeby AutoMigrate się nie wywalił
	if gdb.Migrator().HasTable(&LinkIssue{}) {
		// HARD DELETE: usuń wszystko, łącznie z soft-deleted (deleted_at)
		if err := gdb.Unscoped().Where("1=1").Delete(&LinkIssue{}).Error; err != nil {
			return fmt.Errorf("hard purge link_issues failed: %w", err)
		}
		// Zdejmij stary indeks, jeśli istnieje (bez błędu jeśli go nie ma)
		_ = gdb.Exec(`DROP INDEX IF EXISTS uniq_issue_key;`).Error
	}

	// 2) AutoMigrate – dopiero teraz (gdy nie ma duplikatów) może bezpiecznie tworzyć indeksy z tagów
	if err := gdb.AutoMigrate(
		&ImportFile{},
		&StProduct{},
		&StStock{},
		&WooProductCache{},
		&WooTask{},
		&KV{},
		&LinkIssue{},
	); err != nil {
		return fmt.Errorf("AutoMigrate error: %w", err)
	}

	// 3) Jeżeli w modelu LinkIssue NIE masz tagów unique dla wspólnego indeksu,
	//    to możesz wymusić indeks tutaj. Jeśli tagi masz – ten blok można pominąć.
	//    Zostawiam defensywnie:
	if !gdb.Migrator().HasIndex(&LinkIssue{}, "uniq_issue_key") {
		if err := gdb.Exec(`
			CREATE UNIQUE INDEX IF NOT EXISTS uniq_issue_key
			ON link_issues(towar_id, reason, kod);
		`).Error; err != nil {
			return fmt.Errorf("create index uniq_issue_key: %w", err)
		}
	}

	return nil
}
