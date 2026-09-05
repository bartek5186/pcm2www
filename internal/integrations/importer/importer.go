package importer

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bartek5186/pcm2www/internal/db"
	"github.com/bartek5186/pcm2www/internal/integrations"
	"github.com/rs/zerolog"
	"golang.org/x/net/html/charset"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Config struct {
	WatchDir         string `json:"watch_dir"`                   // np. ~/pcm2www/imports
	PollSec          int    `json:"poll_sec"`                    // np. 5-10s w dev
	PriceMode        string `json:"price_mode,omitempty"`        // gross (domyślnie) albo net
	StabilitySeconds int    `json:"stability_seconds,omitempty"` // plik musi być niezmienny przez ten czas
}

type Importer struct {
	log zerolog.Logger
	cfg Config

	ctx           context.Context
	cancel        context.CancelFunc
	db            *gorm.DB
	runtime       *integrations.Runtime
	observedFiles map[string]fileObservation
}

type fileObservation struct {
	size      int64
	modified  time.Time
	firstSeen time.Time
}

// minimalny model pod to, co potrzebujesz teraz
type xmlMagazyn struct {
	MagazynID  int64  `xml:"magazyn_id"`
	Stan       string `xml:"stan_magazynu"`     // może być "", więc string
	Rezerwacja string `xml:"rezerwacja_ilosci"` // jw.
}

type xmlTowar struct {
	TowarID     int64  `xml:"towar_id"`
	Kod         string `xml:"kod"`
	Nazwa       string `xml:"nazwa"`
	Opis1       string `xml:"opis1"`
	VatID       int64  `xml:"vat_id"`
	KategoriaID string `xml:"kategoria_id"` // bywa puste → string
	GrupaID     string `xml:"asortyment_id"`
	JmID        int64  `xml:"jm_id"`

	DoUsuniecia string `xml:"do_usuniecia"` // "Y"/"N"
	AktywnyWSI  string `xml:"aktywny_w_SI"` // "Y"/"N"

	CenaDetal     string `xml:"cena_detal"`
	CenaHurtowa   string `xml:"cena_hurtowa"`
	CenaNocna     string `xml:"cena_nocna"`
	CenaDodatkowa string `xml:"cena_dodatkowa"`
	CenaDetPrzed  string `xml:"cena_detal_przed_prom"`
	NajCena30Det  string `xml:"najnizsza_cena_30_dni_detal,omitempty"` // jeśli masz w eksporcie

	FolderZdjec      string `xml:"folder_zdjec"`
	PlikZdjecia      string `xml:"plik_zdjecia"`
	DataAktualizacji string `xml:"data_aktualizacji"`

	Magazyny []xmlMagazyn `xml:"magazyny>magazyn"`
}

func (i *Importer) Name() string { return "importer" }

func (i *Importer) Start(ctx context.Context) error {
	i.ctx, i.cancel = context.WithCancel(ctx)
	i.log.Info().Str("integration", i.Name()).Msg("start")

	runtime, err := integrations.RuntimeFromContext(ctx)
	if err != nil {
		return fmt.Errorf("importer: %w", err)
	}
	gdb := runtime.DB
	if gdb == nil {
		return errors.New("importer: brak *gorm.DB w kontekście")
	}
	i.db = gdb
	i.runtime = runtime
	if i.observedFiles == nil {
		i.observedFiles = make(map[string]fileObservation)
	}

	dir := expandHome(i.cfg.WatchDir)
	if info, err := os.Stat(dir); err != nil {
		return fmt.Errorf("importer: katalog %q: %w", dir, err)
	} else if !info.IsDir() {
		return fmt.Errorf("importer: %q nie jest katalogiem", dir)
	}
	ticker := time.NewTicker(i.interval())
	defer ticker.Stop()

	// pierwszy przebieg
	i.scanOnce(dir)
	ready := runtime.WooCacheReady()
	reconcileCurrent := true
	reconcile := func() {
		if !runtime.IsWooCacheReady() || !reconcileCurrent {
			return
		}
		if err := i.linkAndPlanCurrentStaging(); err != nil {
			i.log.Error().Err(err).Msg("startup link/plan failed; will retry next poll")
			return
		}
		reconcileCurrent = false
	}
	if runtime.IsWooCacheReady() {
		reconcile()
		ready = nil
	}

	for {
		select {
		case <-i.ctx.Done():
			i.log.Info().Str("integration", i.Name()).Msg("stop")
			return nil
		case <-ready:
			ready = nil
			reconcile()
		case <-ticker.C:
			i.scanOnce(dir)
			reconcile()
			ticker.Reset(i.interval())
		}
	}
}

func (i *Importer) linkAndPlanCurrentStaging() error {
	// On startup reconcile current staging too, including databases predating
	// PlanningPending. Persist the request before linking so failures are retried.
	if err := i.db.Model(&db.ImportFile{}).
		Where("status = ? AND import_id IN (?)", 1, i.db.Model(&db.StProduct{}).Select("import_id")).
		Update("planning_pending", true).Error; err != nil {
		return fmt.Errorf("mark current staging for planning: %w", err)
	}
	return i.planPendingImports()
}

func (i *Importer) planPendingImports() error {
	var ids []uint
	if err := i.db.Model(&db.ImportFile{}).
		Where("status = ? AND planning_pending = ?", 1, true).
		Order("import_id ASC").Pluck("import_id", &ids).Error; err != nil {
		return fmt.Errorf("load pending planning imports: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}
	if err := i.LinkProductsByEAN(); err != nil {
		return err
	}
	return i.PlanWooTasksForImports(ids)
}

func (i *Importer) Stop() {
	if i.cancel != nil {
		i.cancel()
	}
}

func (i *Importer) interval() time.Duration {
	if i.cfg.PollSec <= 0 {
		return 10 * time.Second
	}
	return time.Duration(i.cfg.PollSec) * time.Second
}

func (i *Importer) scanOnce(dir string) {
	// Do not overwrite the staging history of an import whose planning failed.
	// Resume its durable request before consuming another XML snapshot.
	if !i.resumePlanning() {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		i.log.Error().Err(err).Str("dir", dir).Msg("nie mogę odczytać katalogu")
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !isImportXMLName(name) {
			continue
		}
		full := filepath.Join(dir, name)
		if !i.fileReady(full) {
			continue
		}

		// dedup po SHA256 lub niepustym transmisja_id
		importID, already, err := i.registerFile(full, name)
		if err != nil {
			i.log.Error().Err(err).Str("file", name).Msg("rejestracja pliku nieudana")
			continue
		}

		if already {
			// sprawdź status — jeśli != done (1), to reprocess
			var rec db.ImportFile
			if err := i.db.Where("import_id = ?", importID).Take(&rec).Error; err == nil {
				if rec.Status != 1 {
					i.log.Warn().Str("file", name).Uint("import_id", importID).
						Int("status", rec.Status).Msg("plik istnieje, ale nie DONE — ponawiam przetwarzanie")
					// leć dalej do processFile
				} else {
					//i.log.Debug().Str("file", name).Msg("plik już był i DONE — pomijam")
					if archivedPath, err := archiveProcessedFile(dir, full, name, importID); err != nil {
						i.log.Error().Err(err).Str("file", name).Msg("archiwizacja już przetworzonego pliku nieudana")
					} else {
						delete(i.observedFiles, full)
						i.log.Info().Str("file", name).Str("archived_path", archivedPath).Msg("przeniesiono już przetworzony plik do parsed")
					}
					continue
				}
			} else {
				// nie znalazłem? przetwarzaj ostrożnie
				i.log.Warn().Str("file", name).Msg("brak rekordu import_files dla istniejącego pliku — przetwarzam")
			}
		}

		// PRZETWARZANIE
		if err := i.processFile(importID, full); err != nil {
			i.log.Error().Err(err).Str("file", name).Uint("import_id", importID).Msg("błąd przetwarzania pliku")
			if persistErr := i.db.Model(&db.ImportFile{}).Where("import_id = ?", importID).
				Updates(map[string]any{"status": 2, "last_error": err.Error()}).Error; persistErr != nil {
				i.log.Error().Err(persistErr).Uint("import_id", importID).Msg("nie można zapisać błędu importu")
			}
			continue
		}
		archivedPath, archiveErr := archiveProcessedFile(dir, full, name, importID)
		if archiveErr != nil {
			i.log.Error().Err(archiveErr).Str("file", name).Uint("import_id", importID).Msg("archiwizacja przetworzonego pliku nieudana")
		}
		delete(i.observedFiles, full)

		i.log.Info().Str("file", name).Str("archived_path", archivedPath).Uint("import_id", importID).Msg("przetworzono OK")
		if !i.resumePlanning() {
			return
		}
	}
}

func (i *Importer) resumePlanning() bool {
	if i.runtime != nil && !i.runtime.IsWooCacheReady() {
		var pending int64
		if err := i.db.Model(&db.ImportFile{}).Where("status = ? AND planning_pending = ?", 1, true).Count(&pending).Error; err != nil {
			i.log.Error().Err(err).Msg("cannot check pending planning")
			return false
		}
		return pending == 0
	}
	if err := i.planPendingImports(); err != nil {
		i.log.Error().Err(err).Msg("link/plan pending imports failed; will retry next poll")
		return false
	}
	return true
}

func (i *Importer) fileReady(path string) bool {
	if i.cfg.StabilitySeconds <= 0 {
		return true
	}
	if i.observedFiles == nil {
		i.observedFiles = make(map[string]fileObservation)
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	now := time.Now()
	previous, ok := i.observedFiles[path]
	if !ok || previous.size != info.Size() || !previous.modified.Equal(info.ModTime()) {
		i.observedFiles[path] = fileObservation{size: info.Size(), modified: info.ModTime(), firstSeen: now}
		return false
	}
	return now.Sub(previous.firstSeen) >= time.Duration(i.cfg.StabilitySeconds)*time.Second
}

func isImportXMLName(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return strings.HasPrefix(name, "exp_wyk_") && strings.HasSuffix(name, ".xml")
}

func archiveProcessedFile(dir, fullPath, name string, importID uint) (string, error) {
	parsedDir := filepath.Join(dir, "parsed")
	if err := os.MkdirAll(parsedDir, 0o755); err != nil {
		return "", err
	}

	dest := filepath.Join(parsedDir, name)
	if _, err := os.Stat(dest); err == nil {
		ext := filepath.Ext(name)
		base := strings.TrimSuffix(name, ext)
		dest = filepath.Join(parsedDir, base+".import_"+strconv.FormatUint(uint64(importID), 10)+ext)
		for n := 1; ; n++ {
			if _, err := os.Stat(dest); os.IsNotExist(err) {
				break
			} else if err != nil {
				return "", err
			}
			dest = filepath.Join(parsedDir, base+".import_"+strconv.FormatUint(uint64(importID), 10)+"."+strconv.Itoa(n)+ext)
		}
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}

	if err := os.Rename(fullPath, dest); err != nil {
		return "", err
	}
	return dest, nil
}

func (i *Importer) registerFile(fullPath, name string) (uint, bool, error) {
	fi, err := os.Stat(fullPath)
	if err != nil {
		return 0, false, err
	}

	h, err := fileSHA256(fullPath)
	if err != nil {
		return 0, false, err
	}

	// Odczytaj transmisja_id z obsługiwanego XML-a.
	transID := ""
	if tid, _ := readTransmisjaID(fullPath); tid != "" {
		transID = tid
	}

	rec := db.ImportFile{
		Filename:     name,
		FileTimeUTC:  inferTimeFromName(name),
		TransmisjaID: transID,
		SHA256:       h,
		SizeBytes:    fi.Size(),
		Status:       0,
	}

	// Tożsamością pliku jest zawartość albo niepuste ID transmisji. Nazwa jest
	// tylko metadanym i może zostać ponownie użyta przez PC-Market.
	existing, found, err := findRegisteredFile(i.db, h, transID)
	if err != nil {
		return 0, false, err
	}
	if found {
		if existing.Status != 1 && existing.SHA256 != h {
			if err := i.db.Model(&db.ImportFile{}).Where("import_id = ?", existing.ImportID).Updates(map[string]any{
				"filename": name, "sha256": h, "size_bytes": fi.Size(), "file_time_utc": rec.FileTimeUTC,
			}).Error; err != nil {
				return 0, false, fmt.Errorf("refresh failed import identity: %w", err)
			}
		}
		return existing.ImportID, true, nil
	}

	if err := i.db.Create(&rec).Error; err != nil {
		// SHA ma indeks unikalny. Jeśli drugi proces wygrał wyścig między SELECT
		// i INSERT, odczytaj utworzony przez niego rekord jako duplikat.
		existing, found, lookupErr := findRegisteredFile(i.db, h, transID)
		if lookupErr != nil {
			return 0, false, fmt.Errorf("lookup after import registration failure: %w (insert: %v)", lookupErr, err)
		}
		if found {
			return existing.ImportID, true, nil
		}
		return 0, false, err
	}
	return rec.ImportID, false, nil
}

func findRegisteredFile(gdb *gorm.DB, sha256, transmissionID string) (db.ImportFile, bool, error) {
	bySHA, foundSHA, err := lookupImportFile(gdb, "sha256 = ?", sha256)
	if err != nil {
		return db.ImportFile{}, false, err
	}
	if transmissionID == "" {
		return bySHA, foundSHA, nil
	}

	byTransmission, foundTransmission, err := lookupImportFile(gdb, "transmisja_id = ?", transmissionID)
	if err != nil {
		return db.ImportFile{}, false, err
	}
	if foundSHA && foundTransmission && bySHA.ImportID != byTransmission.ImportID {
		return db.ImportFile{}, false, fmt.Errorf(
			"conflicting import identity: sha256 belongs to import_id=%d, transmisja_id=%q belongs to import_id=%d",
			bySHA.ImportID, transmissionID, byTransmission.ImportID,
		)
	}
	if foundSHA {
		return bySHA, true, nil
	}
	return byTransmission, foundTransmission, nil
}

func lookupImportFile(gdb *gorm.DB, query string, arg any) (db.ImportFile, bool, error) {
	var existing db.ImportFile
	err := gdb.Where(query, arg).Order("import_id ASC").Take(&existing).Error
	if err == nil {
		return existing, true, nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return db.ImportFile{}, false, nil
	}
	return db.ImportFile{}, false, err
}

func (i *Importer) processFile(importID uint, fullPath string) error {
	f, err := os.Open(fullPath)
	if err != nil {
		return err
	}
	defer f.Close()

	br := bufio.NewReader(f)
	dec := xml.NewDecoder(br)
	dec.CharsetReader = func(cs string, in io.Reader) (io.Reader, error) {
		return charset.NewReaderLabel(normalizeCharset(cs), in)
	}

	insProducts, insStocks := 0, 0
	if err := i.db.Transaction(func(tx *gorm.DB) error {
		const batchSize = 500
		prodBatch := make([]db.StProduct, 0, batchSize)
		stockBatch := make([]db.StStock, 0, batchSize)
		type stockKey struct {
			TowarID   int64
			MagazynID int64
		}
		seenProducts := make(map[int64]struct{})
		seenStocks := make(map[stockKey]struct{})

		// ✅ Upsert w batchach
		flushBatches := func(tx *gorm.DB) error {
			// ---- produkty ----
			if len(prodBatch) > 0 {
				err := tx.Clauses(clause.OnConflict{
					Columns: []clause.Column{{Name: "towar_id"}},
					DoUpdates: clause.Assignments(map[string]interface{}{
						"kod":                 gorm.Expr("excluded.kod"),
						"nazwa":               gorm.Expr("excluded.nazwa"),
						"opis1":               gorm.Expr("excluded.opis1"),
						"vat_id":              gorm.Expr("excluded.vat_id"),
						"kategoria_id":        gorm.Expr("excluded.kategoria_id"),
						"grupa_id":            gorm.Expr("excluded.grupa_id"),
						"jm_id":               gorm.Expr("excluded.jm_id"),
						"cena_detal":          gorm.Expr("excluded.cena_detal"),
						"cena_hurtowa":        gorm.Expr("excluded.cena_hurtowa"),
						"cena_nocna":          gorm.Expr("excluded.cena_nocna"),
						"cena_dodatkowa":      gorm.Expr("excluded.cena_dodatkowa"),
						"cena_det_przed_prom": gorm.Expr("excluded.cena_det_przed_prom"),
						"naj_cena30_det":      gorm.Expr("excluded.naj_cena30_det"),
						"aktywny_wsi":         gorm.Expr("excluded.aktywny_wsi"),
						"do_usuniecia":        gorm.Expr("excluded.do_usuniecia"),
						"data_aktualizacji":   gorm.Expr("excluded.data_aktualizacji"),
						"folder_zdjec":        gorm.Expr("excluded.folder_zdjec"),
						"plik_zdjecia":        gorm.Expr("excluded.plik_zdjecia"),
						"import_id":           gorm.Expr("excluded.import_id"),
						"updated_at":          gorm.Expr("CURRENT_TIMESTAMP"),
					}),
				}).Create(&prodBatch).Error
				if err != nil {
					i.log.Error().Err(err).Int("n", len(prodBatch)).Msg("upsert st_products batch failed")
					return err
				}
				insProducts += len(prodBatch)
				prodBatch = prodBatch[:0]
			}

			// ---- stany ----
			if len(stockBatch) > 0 {
				err := tx.Clauses(clause.OnConflict{
					Columns: []clause.Column{{Name: "towar_id"}, {Name: "magazyn_id"}},
					DoUpdates: clause.Assignments(map[string]interface{}{
						"stan_prev":       gorm.Expr("stan"),
						"rezerwacja_prev": gorm.Expr("rezerwacja"),
						"stan":            gorm.Expr("excluded.stan"),
						"rezerwacja":      gorm.Expr("excluded.rezerwacja"),
						"import_id":       gorm.Expr("excluded.import_id"),
						"updated_at":      gorm.Expr("CURRENT_TIMESTAMP"),
					}),
				}).Create(&stockBatch).Error
				if err != nil {
					i.log.Error().Err(err).Int("n", len(stockBatch)).Msg("upsert st_stock batch failed")
					return err
				}
				insStocks += len(stockBatch)
				stockBatch = stockBatch[:0]
			}
			return nil
		}

		for {
			tok, err := dec.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}

			switch se := tok.(type) {
			case xml.StartElement:
				// transmisja_id
				if strings.EqualFold(se.Name.Local, "transmisja_id") {
					var tid string
					if err := dec.DecodeElement(&tid, &se); err != nil {
						return err
					}
					tid = strings.TrimSpace(tid)
					if tid != "" {
						if err := tx.Model(&db.ImportFile{}).
							Where("import_id = ?", importID).
							Update("transmisja_id", tid).Error; err != nil {
							return fmt.Errorf("update transmisja_id: %w", err)
						}
					}
					continue
				}

				// towary
				if strings.EqualFold(se.Name.Local, "towary") {
					var tw struct {
						Items []xmlTowar `xml:"towar"`
					}
					if err := dec.DecodeElement(&tw, &se); err != nil {
						return err
					}

					for _, t := range tw.Items {
						kod := strings.TrimSpace(t.Kod)
						if _, duplicate := seenProducts[t.TowarID]; duplicate {
							return fmt.Errorf("duplicate product in XML: towar_id=%d (latest kod=%q)", t.TowarID, kod)
						}
						seenProducts[t.TowarID] = struct{}{}

						parseFloat := func(field, raw string) (float64, error) {
							value, err := f64(raw)
							if err != nil {
								return 0, fmt.Errorf("towar_id=%d: invalid %s: %w", t.TowarID, field, err)
							}
							return value, nil
						}

						kategoriaID, err := i64(t.KategoriaID)
						if err != nil {
							return fmt.Errorf("towar_id=%d: invalid kategoria_id: %w", t.TowarID, err)
						}
						grupaID, err := i64(t.GrupaID)
						if err != nil {
							return fmt.Errorf("towar_id=%d: invalid asortyment_id: %w", t.TowarID, err)
						}
						cenaDetal, err := parseFloat("cena_detal", t.CenaDetal)
						if err != nil {
							return err
						}
						cenaHurtowa, err := parseFloat("cena_hurtowa", t.CenaHurtowa)
						if err != nil {
							return err
						}
						cenaNocna, err := parseFloat("cena_nocna", t.CenaNocna)
						if err != nil {
							return err
						}
						cenaDodatkowa, err := parseFloat("cena_dodatkowa", t.CenaDodatkowa)
						if err != nil {
							return err
						}
						cenaDetPrzed, err := parseFloat("cena_detal_przed_prom", t.CenaDetPrzed)
						if err != nil {
							return err
						}
						najCena30Det, err := parseFloat("najnizsza_cena_30_dni_detal", t.NajCena30Det)
						if err != nil {
							return err
						}

						prodBatch = append(prodBatch, db.StProduct{
							ImportID:         importID,
							TowarID:          t.TowarID,
							Kod:              kod,
							Nazwa:            strings.TrimSpace(t.Nazwa),
							Opis1:            t.Opis1,
							VatID:            t.VatID,
							KategoriaID:      kategoriaID,
							GrupaID:          grupaID,
							JmID:             t.JmID,
							CenaDetal:        cenaDetal,
							CenaHurtowa:      cenaHurtowa,
							CenaNocna:        cenaNocna,
							CenaDodatkowa:    cenaDodatkowa,
							CenaDetPrzedProm: cenaDetPrzed,
							NajCena30Det:     najCena30Det,
							AktywnyWSI:       yn(t.AktywnyWSI),
							DoUsuniecia:      yn(t.DoUsuniecia),
							DataAktualizacji: t.DataAktualizacji,
							FolderZdjec:      t.FolderZdjec,
							PlikZdjecia:      t.PlikZdjecia,
						})

						for _, m := range t.Magazyny {
							skey := stockKey{TowarID: t.TowarID, MagazynID: m.MagazynID}
							if _, duplicate := seenStocks[skey]; duplicate {
								return fmt.Errorf("duplicate stock in XML: towar_id=%d magazyn_id=%d", t.TowarID, m.MagazynID)
							}
							seenStocks[skey] = struct{}{}

							stan, err := parseFloat("stan_magazynu", m.Stan)
							if err != nil {
								return err
							}
							rezerwacja, err := parseFloat("rezerwacja_ilosci", m.Rezerwacja)
							if err != nil {
								return err
							}
							stockBatch = append(stockBatch, db.StStock{
								ImportID:   importID,
								TowarID:    t.TowarID,
								MagazynID:  m.MagazynID,
								Stan:       stan,
								Rezerwacja: rezerwacja,
							})
						}

						if len(prodBatch) >= batchSize || len(stockBatch) >= batchSize {
							if err := flushBatches(tx); err != nil {
								return err
							}
						}
					}
				}
			}
		}

		if err := flushBatches(tx); err != nil {
			return err
		}
		var registered db.ImportFile
		if err := tx.Select("sha256").Where("import_id = ?", importID).Take(&registered).Error; err != nil {
			return fmt.Errorf("load registered file hash: %w", err)
		}
		currentHash, err := fileSHA256(fullPath)
		if err != nil {
			return fmt.Errorf("verify XML after parsing: %w", err)
		}
		if currentHash != registered.SHA256 {
			return fmt.Errorf("XML changed while it was being imported")
		}

		processedAt := time.Now()
		if err := tx.Model(&db.ImportFile{}).Where("import_id = ?", importID).Updates(map[string]any{
			"status":           1,
			"last_error":       "",
			"processed_at":     processedAt,
			"planning_pending": true,
		}).Error; err != nil {
			return fmt.Errorf("mark import done: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	i.log.Info().
		Uint("import_id", importID).
		Int("products_upserted", insProducts).
		Int("stocks_upserted", insStocks).
		Msg("XML parsed → staging upsert OK")

	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func inferTimeFromName(name string) string {
	// exp_wyk_xxxx_yyyyMMddHHmmss.xml
	base := strings.TrimSuffix(name, filepath.Ext(name))
	parts := strings.Split(base, "_")
	if len(parts) < 3 {
		return ""
	}
	ts := parts[len(parts)-1]
	if len(ts) != 14 {
		return ""
	}
	return ts[:4] + "-" + ts[4:6] + "-" + ts[6:8] + " " + ts[8:10] + ":" + ts[10:12] + ":" + ts[12:14] + "Z"
}

func readTransmisjaID(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	dec := xml.NewDecoder(f)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "transmisja_id" {
			var v string
			if err := dec.DecodeElement(&v, &se); err != nil {
				return "", err
			}
			return strings.TrimSpace(v), nil
		}
	}
	return "", nil
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

func factory(log zerolog.Logger, raw json.RawMessage) (integrations.Integration, error) {
	var cfg Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.WatchDir) == "" {
		return nil, errors.New("importer: watch_dir is required")
	}
	dir := expandHome(cfg.WatchDir)
	if info, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("importer: watch_dir %q: %w", dir, err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("importer: watch_dir %q is not a directory", dir)
	}
	if cfg.PollSec < 0 {
		return nil, errors.New("importer: poll_sec cannot be negative")
	}
	if cfg.StabilitySeconds < 0 {
		return nil, errors.New("importer: stability_seconds cannot be negative")
	}
	if cfg.StabilitySeconds == 0 {
		cfg.StabilitySeconds = 2
	}
	mode, err := normalizePriceMode(cfg.PriceMode)
	if err != nil {
		return nil, err
	}
	cfg.PriceMode = mode
	return &Importer{log: log, cfg: cfg}, nil
}

func init() {
	integrations.Register("importer", factory)
}

// normalizeCharset mapuje nietypowe etykiety na standardowe nazwy rozpoznawane przez charset.NewReaderLabel
func normalizeCharset(cs string) string {
	c := strings.TrimSpace(strings.ToLower(cs))
	switch c {
	case "latin ii", "latin-2", "latin2", "iso8859-2", "iso_8859-2":
		return "iso-8859-2"
	case "cp1250", "windows1250", "win-1250":
		return "windows-1250"
	default:
		return c
	}
}

func yn(s string) bool {
	switch strings.TrimSpace(strings.ToUpper(s)) {
	case "Y", "T", "1", "TAK":
		return true
	default:
		return false
	}
}

func f64(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	// zamień ewentualny przecinek na kropkę
	s = strings.ReplaceAll(s, ",", ".")
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("non-finite number %q", s)
	}
	return v, nil
}

func i64(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	return strconv.ParseInt(s, 10, 64)
}
