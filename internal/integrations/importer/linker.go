package importer

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/bartek5186/pcm2www/internal/db"
)

var reDigits = regexp.MustCompile(`\D+`)

func cleanEAN(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return reDigits.ReplaceAllString(s, "")
}

// LinkProductsByEAN: st_products.kod ↔ woo_product_caches.ean (TYLKO po EAN)
func (i *Importer) LinkProductsByEAN(importID uint) error {
	tx := i.db.Begin()

	if !tx.Migrator().HasTable(&db.WooProductCache{}) {
		i.log.Warn().Uint("import_id", importID).Msg("linker: cache table missing, skip (first run)")
		return tx.Commit().Error
	}

	var cacheCount int64
	if err := tx.Model(&db.WooProductCache{}).Count(&cacheCount).Error; err != nil {
		return err
	}
	if cacheCount == 0 {
		i.log.Warn().Uint("import_id", importID).Msg("linker: cache empty, skip (will retry next cycle)")
		return tx.Commit().Error
	}

	defer tx.Rollback()

	// wyczyść stare problemy
	if err := tx.Where("import_id = ?", importID).Delete(&db.LinkIssue{}).Error; err != nil {
		return err
	}

	// staging (tylko pola potrzebne do linkowania)
	var st []struct {
		TowarID int64
		Kod     string
	}
	if err := tx.Model(&db.StProduct{}).
		Select("towar_id", "kod").
		Where("import_id = ?", importID).
		Find(&st).Error; err != nil {
		return err
	}

	// cache Woo (tylko ean + klucz)
	var wc []struct {
		WooID uint
		Ean   string
	}
	if err := tx.Model(&db.WooProductCache{}).
		Select("woo_id", "ean").
		Find(&wc).Error; err != nil {
		return err
	}

	// zlicz statystyki do debug
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
		Uint("import_id", importID).
		Int("staging_items", totalSt).
		Int("cache_items", totalWc).
		Int("cache_empty_ean", emptyEanWc).
		Int("cache_index_keys", len(byEAN)).
		Msg("linker: input stats")

	var (
		issues       []db.LinkIssue
		matchedByEAN int
		// ograniczniki spamu
		maxDbgNoMatch      = 10
		maxDbgMultiMatch   = 10
		maxDbgMatched      = 10
		dbgNoMatchCount    = 0
		dbgMultiMatchCount = 0
		dbgMatchedCount    = 0
	)

	// główna pętla
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
			issues = append(issues, db.LinkIssue{
				ImportID: importID, TowarID: p.TowarID, Kod: p.Kod,
				Reason:  "missing_ean_src",
				Details: "Brak EAN w eksporcie (pole 'kod' puste/niecyfrowe)",
			})
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
			issues = append(issues, db.LinkIssue{
				ImportID: importID, TowarID: p.TowarID, Kod: p.Kod,
				Reason:  "missing_in_shop_by_ean",
				Details: fmt.Sprintf("Brak produktu o EAN=%s w Woo", ean),
			})
		case 1:
			if err := tx.Model(&db.WooProductCache{}).
				Where("woo_id = ?", cands[0]).
				Update("towar_id", p.TowarID).Error; err != nil {
				return err
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
			if dbgMultiMatchCount < maxDbgMultiMatch {
				i.log.Debug().
					Int64("towar_id", p.TowarID).
					Str("ean_clean", ean).
					Int("candidates", len(cands)).
					Interface("woo_ids", cands).
					Msg("linker: MULTI-MATCH by EAN (duplicate EAN in Woo)")
				dbgMultiMatchCount++
			}
			issues = append(issues, db.LinkIssue{
				ImportID: importID, TowarID: p.TowarID, Kod: p.Kod,
				Reason:  "duplicate_ean_shop",
				Details: fmt.Sprintf("EAN=%s występuje %d× w Woo (woo_id: %v)", ean, len(cands), cands),
			})
		}
	}

	// zapisz problemy
	if len(issues) > 0 {
		if err := tx.Create(&issues).Error; err != nil {
			return err
		}
	}

	i.log.Info().
		Uint("import_id", importID).
		Int("matched_by_ean", matchedByEAN).
		Int("issues", len(issues)).
		Int("dbg_no_match_printed", dbgNoMatchCount).
		Int("dbg_multi_match_printed", dbgMultiMatchCount).
		Int("dbg_matched_printed", dbgMatchedCount).
		Msg("EAN linking finished")

	return tx.Commit().Error
}
