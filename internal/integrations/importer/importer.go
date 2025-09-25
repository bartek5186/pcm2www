package importer

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
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
)

type Config struct {
	WatchDir string `json:"watch_dir"` // np. ~/pcm2www/imports
	PollSec  int    `json:"poll_sec"`  // np. 5-10s w dev
}

type Importer struct {
	log zerolog.Logger
	cfg Config

	ctx    context.Context
	cancel context.CancelFunc
	db     *gorm.DB
}

// minimalny model pod to, co potrzebujesz teraz
type xmlMagazyn struct {
	MagazynID  int64  `xml:"magazyn_id"`
	Stan       string `xml:"stan_magazynu"`     // może być "", więc string
	Rezerwacja string `xml:"rezerwacja_ilosci"` // jw.
}

// liczniki
var (
	seenTowar   int
	insProducts int
	insStocks   int
)

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

	// wyciągnij DB z contextu (patrz: Syncer.Start)
	raw := ctx.Value("gormDB")
	gdb, _ := raw.(*gorm.DB)
	if gdb == nil {
		return errors.New("importer: brak *gorm.DB w kontekście")
	}
	i.db = gdb

	dir := expandHome(i.cfg.WatchDir)
	ticker := time.NewTicker(i.interval())
	defer ticker.Stop()

	// pierwszy przebieg
	i.scanOnce(dir)

	for {
		select {
		case <-i.ctx.Done():
			i.log.Info().Str("integration", i.Name()).Msg("stop")
			return nil
		case <-ticker.C:
			i.scanOnce(dir)
			ticker.Reset(i.interval())
		}
	}
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
		if !strings.HasPrefix(name, "exp_wyk_") || !(strings.HasSuffix(name, ".xml") || strings.HasSuffix(name, ".zip")) {
			continue
		}
		full := filepath.Join(dir, name)

		// dedup po filename/sha/transmisja_id
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
					i.log.Debug().Str("file", name).Msg("plik już był i DONE — pomijam")
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
			_ = i.db.Model(&db.ImportFile{}).Where("import_id = ?", importID).
				Updates(map[string]any{"status": 2, "last_error": err.Error()})
			continue
		}

		if err := i.LinkProductsByEAN(importID); err != nil {
			i.log.Error().Err(err).Uint("import_id", importID).Msg("linking failed")
			// nadal możesz ustawić status=1 na ImportFile, ale warto zostawić warning
		}

		// sukces: status=1, processed_at=now
		now := time.Now()
		_ = i.db.Model(&db.ImportFile{}).Where("import_id = ?", importID).
			Updates(map[string]any{"status": 1, "processed_at": now})

		// DEV: usuwanie zakomentowane
		// _ = os.Remove(full)
		i.log.Info().Str("file", name).Uint("import_id", importID).Msg("przetworzono OK")
	}

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

	// spróbuj odczytać transmisja_id (jeśli to XML)
	transID := ""
	if strings.HasSuffix(strings.ToLower(name), ".xml") {
		if tid, _ := readTransmisjaID(fullPath); tid != "" {
			transID = tid
		}
	}

	rec := db.ImportFile{
		Filename:     name,
		FileTimeUTC:  inferTimeFromName(name),
		TransmisjaID: transID,
		SHA256:       h,
		SizeBytes:    fi.Size(),
		Status:       0,
	}

	// idempotencja: po SHA lub nazwie/transmisja_id
	var existing db.ImportFile
	if err := i.db.
		Where("sha256 = ? OR filename = ? OR (transmisja_id <> '' AND transmisja_id = ?)", h, name, transID).
		Take(&existing).Error; err == nil {
		return existing.ImportID, true, nil
	}

	if err := i.db.Create(&rec).Error; err != nil {
		return 0, false, err
	}
	return rec.ImportID, false, nil
}

func (i *Importer) processFile(importID uint, fullPath string) error {
	// stream-parse XML → staging
	f, err := os.Open(fullPath)
	if err != nil {
		return err
	}
	defer f.Close()

	// Buforowany reader + dekoder z obsługą charsetów
	br := bufio.NewReader(f)
	dec := xml.NewDecoder(br)
	dec.CharsetReader = func(cs string, in io.Reader) (io.Reader, error) {
		return charset.NewReaderLabel(normalizeCharset(cs), in)
	}

	const batchSize = 500
	prodBatch := make([]db.StProduct, 0, batchSize)
	stockBatch := make([]db.StStock, 0, batchSize)

	insProducts, insStocks := 0, 0

	flushBatches := func(tx *gorm.DB) error {
		if len(prodBatch) > 0 {
			if err := tx.Create(&prodBatch).Error; err != nil {
				i.log.Error().Err(err).Int("n", len(prodBatch)).Msg("insert st_products batch failed")
				return err
			}
			insProducts += len(prodBatch)
			prodBatch = prodBatch[:0]
		}
		if len(stockBatch) > 0 {
			if err := tx.Create(&stockBatch).Error; err != nil {
				i.log.Error().Err(err).Int("n", len(stockBatch)).Msg("insert st_stock batch failed")
				return err
			}
			insStocks += len(stockBatch)
			stockBatch = stockBatch[:0]
		}
		return nil
	}

	tx := i.db.Begin()
	defer tx.Rollback()

	// wyczyść staging dla tego importu (idempotentnie)
	if err := tx.Where("import_id = ?", importID).Delete(&db.StStock{}).Error; err != nil {
		return err
	}
	if err := tx.Where("import_id = ?", importID).Delete(&db.StProduct{}).Error; err != nil {
		return err
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
			// 1) tylko <transmisja_id> – nie połykać całego <dane>
			if strings.EqualFold(se.Name.Local, "transmisja_id") {
				var tid string
				if err := dec.DecodeElement(&tid, &se); err != nil {
					return err
				}
				tid = strings.TrimSpace(tid)
				if tid != "" {
					_ = tx.Model(&db.ImportFile{}).
						Where("import_id = ?", importID).
						Update("transmisja_id", tid).Error
				}
				continue
			}

			// 2) <towary> – zdekoduj całą listę <towar>
			if strings.EqualFold(se.Name.Local, "towary") {
				var tw struct {
					Items []xmlTowar `xml:"towar"`
				}
				if err := dec.DecodeElement(&tw, &se); err != nil {
					return err
				}

				for _, t := range tw.Items {
					// produkt → st_products (batch)
					prodBatch = append(prodBatch, db.StProduct{
						ImportID:         importID,
						TowarID:          t.TowarID,
						Kod:              strings.TrimSpace(t.Kod),
						Nazwa:            strings.TrimSpace(t.Nazwa),
						Opis1:            t.Opis1,
						VatID:            t.VatID,
						KategoriaID:      i64(t.KategoriaID),
						GrupaID:          i64(t.GrupaID),
						JmID:             t.JmID,
						CenaDetal:        f64(t.CenaDetal),
						CenaHurtowa:      f64(t.CenaHurtowa),
						CenaNocna:        f64(t.CenaNocna),
						CenaDodatkowa:    f64(t.CenaDodatkowa),
						CenaDetPrzedProm: f64(t.CenaDetPrzed),
						NajCena30Det:     f64(t.NajCena30Det),
						AktywnyWSI:       yn(t.AktywnyWSI),
						DoUsuniecia:      yn(t.DoUsuniecia),
						DataAktualizacji: t.DataAktualizacji,
						FolderZdjec:      t.FolderZdjec,
						PlikZdjecia:      t.PlikZdjecia,
					})

					// stany → st_stock (batch)
					for _, m := range t.Magazyny {
						stockBatch = append(stockBatch, db.StStock{
							ImportID:   importID,
							TowarID:    t.TowarID,
							MagazynID:  m.MagazynID,
							Stan:       f64(m.Stan),
							Rezerwacja: f64(m.Rezerwacja),
						})
					}

					// okresowy flush
					if len(prodBatch) >= batchSize || len(stockBatch) >= batchSize {
						if err := flushBatches(tx); err != nil {
							return err
						}
					}
				}
				continue
			}

			// inne sekcje na razie pomijamy
		}
	}

	// flush resztek i commit
	if err := flushBatches(tx); err != nil {
		return err
	}
	if err := tx.Commit().Error; err != nil {
		i.log.Error().Err(err).Msg("tx commit failed")
		return err
	}

	i.log.Info().
		Uint("import_id", importID).
		Int("products_inserted", insProducts).
		Int("stocks_inserted", insStocks).
		Msg("XML parsed → staging OK")
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
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
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

func f64(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	// zamień ewentualny przecinek na kropkę
	s = strings.ReplaceAll(s, ",", ".")
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

func i64(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}
