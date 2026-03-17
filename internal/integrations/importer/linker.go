package importer

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/bartek5186/pcm2www/internal/db"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var reDigits = regexp.MustCompile(`\D+`)

func cleanEAN(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return reDigits.ReplaceAllString(s, "")
}

// LinkProductsByEAN — pełny relink Woo ↔ Magazyn po EAN
// Skasuje i przebuduje całą tabelę link_issues od zera.
func (i *Importer) LinkProductsByEAN() error {
	tx := i.db.Begin()

	// 1️⃣ Wyczyść istniejące problemy (pełny rebuild)
	if err := tx.Unscoped().Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&db.LinkIssue{}).Error; err != nil {
		return fmt.Errorf("błąd czyszczenia link_issues: %w", err)
	}

	// 2️⃣ Upewnij się, że Woo cache istnieje
	if !tx.Migrator().HasTable(&db.WooProductCache{}) {
		i.log.Warn().Msg("linker: Woo cache table missing, skip (first run)")
		return tx.Commit().Error
	}

	// 3️⃣ Sprawdź, czy cache nie jest pusty
	var cacheCount int64
	if err := tx.Model(&db.WooProductCache{}).Count(&cacheCount).Error; err != nil {
		return err
	}
	if cacheCount == 0 {
		i.log.Warn().Msg("linker: Woo cache empty, skip (will retry next cycle)")
		return tx.Commit().Error
	}

	defer tx.Rollback()

	// pełny rebuild powiązań musi zacząć od wyczyszczenia starych linków,
	// inaczej planner może pracować na nieaktualnych TowarID z poprzednich przebiegów.
	if err := tx.Model(&db.WooProductCache{}).
		Session(&gorm.Session{AllowGlobalUpdate: true}).
		Update("towar_id", nil).Error; err != nil {
		return fmt.Errorf("błąd czyszczenia woo_product_caches.towar_id: %w", err)
	}

	// 4️⃣ Wczytaj staging (produkty z magazynu)
	var st []struct {
		TowarID int64
		Kod     string
	}
	if err := tx.Model(&db.StProduct{}).
		Select("towar_id", "kod").
		Find(&st).Error; err != nil {
		return fmt.Errorf("błąd odczytu st_products: %w", err)
	}

	// 5️⃣ Wczytaj Woo cache (produkty z Woo)
	var wc []struct {
		WooID uint
		Ean   string
	}
	if err := tx.Model(&db.WooProductCache{}).
		Select("woo_id", "ean").
		Find(&wc).Error; err != nil {
		return fmt.Errorf("błąd odczytu woo_product_caches: %w", err)
	}

	// 6️⃣ Przygotuj indeks Woo po EAN
	totalSt := len(st)
	totalWc := len(wc)
	emptyEanWc := 0
	byEAN := make(map[string][]uint, len(wc))
	for _, c := range wc {
		e := cleanEAN(c.Ean)
		if e == "" {
			emptyEanWc++
			continue
		}
		byEAN[e] = append(byEAN[e], c.WooID)
	}

	i.log.Debug().
		Int("staging_items", totalSt).
		Int("cache_items", totalWc).
		Int("cache_empty_ean", emptyEanWc).
		Int("cache_index_keys", len(byEAN)).
		Msg("linker: input stats")

	// 7️⃣ Główne statystyki diagnostyczne
	var (
		matchedByEAN       int
		maxDbgNoMatch      = 10
		maxDbgMultiMatch   = 10
		maxDbgMatched      = 10
		dbgNoMatchCount    = 0
		dbgMultiMatchCount = 0
		dbgMatchedCount    = 0
	)

	// 8️⃣ Pętla po produktach magazynowych
	for _, p := range st {
		rawKod := strings.TrimSpace(p.Kod)
		ean := cleanEAN(p.Kod)

		if ean == "" {
			if dbgNoMatchCount < maxDbgNoMatch {
				i.log.Debug().
					Int64("towar_id", p.TowarID).
					Str("kod_raw", rawKod).
					Msg("linker: EMPTY EAN in staging (kod after clean is empty)")
				dbgNoMatchCount++
			}
			saveLinkIssue(tx, p.TowarID, p.Kod, "",
				"missing_ean_src",
				"Brak EAN w eksporcie (pole 'kod' puste/niecyfrowe)")
			continue
		}

		cands := byEAN[ean]
		switch len(cands) {
		case 0:
			if dbgNoMatchCount < maxDbgNoMatch {
				i.log.Debug().
					Int64("towar_id", p.TowarID).
					Str("kod_raw", rawKod).
					Str("ean_clean", ean).
					Msg("linker: NO MATCH in cache by EAN")
				dbgNoMatchCount++
			}
			saveLinkIssue(tx, p.TowarID, p.Kod, "",
				"missing_in_shop_by_ean",
				fmt.Sprintf("Brak produktu o EAN=%s w Woo", ean))

		case 1:
			if err := tx.Model(&db.WooProductCache{}).
				Where("woo_id = ?", cands[0]).
				Update("towar_id", p.TowarID).Error; err != nil {
				return fmt.Errorf("update Woo towar_id=%d error: %w", p.TowarID, err)
			}
			matchedByEAN++
			if dbgMatchedCount < maxDbgMatched {
				i.log.Debug().
					Int64("towar_id", p.TowarID).
					Str("ean_clean", ean).
					Uint("woo_id", cands[0]).
					Msg("linker: MATCHED by EAN")
				dbgMatchedCount++
			}

		default:
			idsJSON, _ := json.Marshal(cands)
			if dbgMultiMatchCount < maxDbgMultiMatch {
				i.log.Debug().
					Int64("towar_id", p.TowarID).
					Str("ean_clean", ean).
					Int("candidates", len(cands)).
					Interface("woo_ids", cands).
					Msg("linker: MULTI-MATCH by EAN (duplicate EAN in Woo)")
				dbgMultiMatchCount++
			}
			saveLinkIssue(tx, p.TowarID, p.Kod, string(idsJSON),
				"duplicate_ean_shop",
				fmt.Sprintf("EAN=%s występuje %d× w Woo (woo_id: %v)", ean, len(cands), cands))
		}
	}

	// 9️⃣ Zbuduj zestaw EANów z magazynu (dla odwrotnego porównania)
	magEans := make(map[string]struct{}, len(st))
	for _, p := range st {
		ean := cleanEAN(p.Kod)
		if ean != "" {
			magEans[ean] = struct{}{}
		}
	}

	// 🔟 Przeskanuj Woo → znajdź produkty nieobecne w magazynie
	missingInMag := 0
	dbgPrinted := 0
	const maxDbgMissing = 10

	for _, w := range wc {
		ean := cleanEAN(w.Ean)
		if ean == "" {
			continue // pomiń produkty Woo bez EAN
		}
		if _, exists := magEans[ean]; !exists {
			missingInMag++
			if dbgPrinted < maxDbgMissing {
				i.log.Debug().
					Str("ean", ean).
					Uint("woo_id", w.WooID).
					Msg("linker: PRODUCT IN WOO but missing in MAGAZYN by EAN")
				dbgPrinted++
			}
			idsJSON := fmt.Sprintf("[%d]", w.WooID)
			saveLinkIssue(tx, 0, ean, idsJSON,
				"missing_in_magazine_by_ean",
				fmt.Sprintf("Produkt o EAN=%s jest w Woo (woo_id=%d), ale nie ma go w magazynie", ean, w.WooID))
		}
	}

	// 1️⃣1️⃣ Podsumowanie
	i.log.Info().
		Int("matched_by_ean", matchedByEAN).
		Int("missing_in_magazine_by_ean", missingInMag).
		Int("dbg_no_match_printed", dbgNoMatchCount).
		Int("dbg_multi_match_printed", dbgMultiMatchCount).
		Int("dbg_matched_printed", dbgMatchedCount).
		Msg("EAN linking finished")

	return tx.Commit().Error
}

// saveLinkIssue – zapisuje pojedynczy problem w linkowaniu
func saveLinkIssue(tx *gorm.DB, towarID int64, kod, wooIDs, reason, details string) {
	issue := db.LinkIssue{
		TowarID: towarID,
		Kod:     kod,
		WooIDs:  wooIDs,
		Reason:  reason,
		Details: details,
	}

	err := tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "towar_id"},
			{Name: "reason"},
			{Name: "kod"},
		},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"woo_ids":    wooIDs,
			"details":    details,
			"updated_at": time.Now(),
		}),
	}).Create(&issue).Error

	if err != nil {
		fmt.Printf("saveLinkIssue: upsert error for towar_id=%d reason=%s: %v\n", towarID, reason, err)
	}
}
