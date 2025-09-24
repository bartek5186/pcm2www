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
			i.log.Debug().Str("file", name).Msg("plik już był – pomijam")
			continue
		}

		// przetwarzaj
		if err := i.processFile(importID, full); err != nil {
			i.log.Error().Err(err).Str("file", name).Msg("błąd przetwarzania pliku")
			// status=error, pliku nie usuwamy
			_ = i.db.Model(&db.ImportFile{}).Where("import_id = ?", importID).
				Updates(map[string]any{"status": 2, "last_error": err.Error()})
			continue
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

	// Buforowany reader (bezpieczniej dla detekcji nagłówka i filtrów znakowych)
	br := bufio.NewReader(f)
	dec := xml.NewDecoder(br)

	dec.CharsetReader = func(cs string, in io.Reader) (io.Reader, error) {
		// mapujemy „Latin II” → „iso-8859-2”, itp.
		return charset.NewReaderLabel(normalizeCharset(cs), in)
	}

	// PRZYKŁAD: czytamy tylko <dane>/<transmisja_id> i liczymy towarów,
	// w praktyce tutaj wstawiasz pełny parsing sekcji i batch insert do st_* tabel
	type daneHeader struct {
		TransmisjaID string `xml:"transmisja_id"`
	}
	var (
		//inWykazy bool
		prodCnt int
	)
	tx := i.db.Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			tx.Rollback()
			return err
		}

		switch se := tok.(type) {
		case xml.StartElement:
			if se.Name.Local == "dane" {
				var d daneHeader
				if err := dec.DecodeElement(&d, &se); err != nil {
					tx.Rollback()
					return err
				}
				// ewentualna aktualizacja transmisja_id jeśli nie było
				if d.TransmisjaID != "" {
					_ = tx.Model(&db.ImportFile{}).
						Where("import_id = ?", importID).
						Update("transmisja_id", d.TransmisjaID).Error
				}
			} else if se.Name.Local == "towar" {
				// TODO: DecodeElement → db.StProduct (INSERT)
				// na razie licznik:
				prodCnt++
			} else if se.Name.Local == "magazyn_stan" {
				// TODO: wstaw do st_stock
			} else if se.Name.Local == "wykazy" {
				//inWykazy = true
			}
		case xml.EndElement:
			if se.Name.Local == "wykazy" {
				//inWykazy = false
			}
		}
	}

	if err := tx.Commit().Error; err != nil {
		return err
	}
	i.log.Info().Uint("import_id", importID).Int("products_seen", prodCnt).Msg("XML parsed (stub)")
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
