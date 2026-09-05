// Package problems builds a read-only report from the current local data.
package problems

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bartek5186/pcm2www/internal/db"
	"github.com/bartek5186/pcm2www/internal/integrations"
	"gorm.io/gorm"
)

type Row struct {
	Category, Code, Problem, Details     string
	PCMName, PCMCode, WooName, WooEAN    string
	PCMID                                int64
	WooID, TaskID, ImportID              uint
	Operation, File, ProductURL, EditURL string
}

type Snapshot struct {
	At                 time.Time
	PCMCount, WooCount int
	Rows               []Row
}

// Load recomputes EAN diagnostics instead of using link_issues, which may
// describe an older linker run. No linking, planning or Woo API writes occur.
func Load(ctx context.Context, gdb *gorm.DB, baseURL string) (Snapshot, error) {
	var out Snapshot
	var pcm []db.StProduct
	var woo []db.WooProductCache
	var tasks []db.WooTask
	var imports []db.ImportFile
	err := gdb.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Select("towar_id", "kod", "nazwa", "import_id").Find(&pcm).Error; err != nil {
			return err
		}
		if err := tx.Select("woo_id", "ean", "name").Find(&woo).Error; err != nil {
			return err
		}
		if err := tx.Select("task_id", "revision", "import_id", "towar_id", "woo_id", "kind", "status", "last_error").
			Order("revision DESC, task_id DESC").Find(&tasks).Error; err != nil {
			return err
		}
		return tx.Select("import_id", "filename", "status", "last_error").Find(&imports).Error
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return out, fmt.Errorf("odczyt problemów: %w", err)
	}
	out.At, out.PCMCount, out.WooCount = time.Now(), len(pcm), len(woo)
	pcmByEAN := make(map[string][]db.StProduct)
	wooByEAN := make(map[string][]db.WooProductCache)
	pcmByID := make(map[int64]db.StProduct)
	wooByID := make(map[uint]db.WooProductCache)
	files := make(map[uint]string)
	for _, p := range pcm {
		pcmByID[p.TowarID] = p
		if e := integrations.NormalizeEAN(p.Kod); e != "" {
			pcmByEAN[e] = append(pcmByEAN[e], p)
		}
	}
	for _, p := range woo {
		wooByID[p.WooID] = p
		if e := integrations.NormalizeEAN(p.Ean); e != "" {
			wooByEAN[e] = append(wooByEAN[e], p)
		}
	}
	for _, f := range imports {
		files[f.ImportID] = f.Filename
	}
	add := func(category, code, title, detail string, p db.StProduct, w db.WooProductCache) {
		out.Rows = append(out.Rows, Row{Category: category, Code: code, Problem: title, Details: detail,
			PCMID: p.TowarID, PCMName: p.Nazwa, PCMCode: p.Kod, WooID: w.WooID, WooName: w.Name, WooEAN: w.Ean,
			ImportID: p.ImportID, File: files[p.ImportID]})
	}
	for _, p := range pcm {
		e := integrations.NormalizeEAN(p.Kod)
		if e == "" {
			add("EAN", "missing_ean_src", "Brak EAN w PCM", "Pole kod jest puste lub nie zawiera cyfr.", p, db.WooProductCache{})
			continue
		}
		if len(pcmByEAN[e]) > 1 {
			detail := fmt.Sprintf("EAN %s występuje przy %d produktach PCM. Powiązanie jest niejednoznaczne.", e, len(pcmByEAN[e]))
			if len(wooByEAN[e]) == 0 {
				add("EAN", "duplicate_ean_source", "Powielony EAN w PCM", detail, p, db.WooProductCache{})
			}
			for _, w := range wooByEAN[e] {
				add("EAN", "duplicate_ean_source", "Powielony EAN w PCM", detail, p, w)
			}
			continue
		}
		if len(wooByEAN[e]) == 0 {
			add("EAN", "missing_in_shop_by_ean", "Brak dopasowania w sklepie", "Nie ma produktu z takim EAN w lokalnych danych WooCommerce.", p, db.WooProductCache{})
		}
	}
	for _, w := range woo {
		e := integrations.NormalizeEAN(w.Ean)
		if e == "" {
			add("EAN", "missing_ean_shop", "Brak EAN w sklepie", "Produkt WooCommerce nie ma EAN zawierającego cyfry.", db.StProduct{}, w)
			continue
		}
		if len(wooByEAN[e]) > 1 {
			detail := fmt.Sprintf("EAN %s występuje przy %d produktach WooCommerce.", e, len(wooByEAN[e]))
			if len(pcmByEAN[e]) == 0 {
				add("EAN", "duplicate_ean_shop", "Powielony EAN w sklepie", detail, db.StProduct{}, w)
			}
			for _, p := range pcmByEAN[e] {
				add("EAN", "duplicate_ean_shop", "Powielony EAN w sklepie", detail, p, w)
			}
		}
		if len(pcmByEAN[e]) == 0 {
			add("EAN", "missing_in_magazine_by_ean", "Produkt sklepu bez dopasowania w PCM", "Brak tego EAN w zaimportowanych danych PCM; nie jest to informacja o zerowym stanie magazynowym.", db.StProduct{}, w)
		}
	}
	// Old errors remain in task history. Only the newest intent per Woo
	// product and operation can represent a current problem. A cancelled
	// newer intent must not resurrect an error from older history either.
	seen := make(map[string]bool)
	for _, t := range tasks {
		key := fmt.Sprintf("task:%d", t.TaskID)
		if t.WooID != nil {
			key = fmt.Sprintf("woo:%d:%s", *t.WooID, t.Kind)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		code, title := "", ""
		switch {
		case t.Status == "error":
			code, title = "task_error", "Błąd aktualizacji produktu"
		case t.Status == "pending" && t.LastError != "":
			code, title = "task_retry", "Aktualizacja oczekuje na ponowienie"
		case t.Status == "skipped" && t.LastError != "":
			code, title = "task_skipped", "Pominięta aktualizacja"
		default:
			continue
		}
		var p db.StProduct
		var w db.WooProductCache
		if t.TowarID != nil {
			p = pcmByID[*t.TowarID]
			p.TowarID = *t.TowarID
		}
		if t.WooID != nil {
			w = wooByID[*t.WooID]
			w.WooID = *t.WooID
		}
		add("Synchronizacja", code, title, t.LastError, p, w)
		r := &out.Rows[len(out.Rows)-1]
		r.TaskID, r.Operation, r.ImportID, r.File = t.TaskID, t.Kind, t.ImportID, files[t.ImportID]
	}
	for _, f := range imports {
		if f.Status == 2 {
			out.Rows = append(out.Rows, Row{Category: "Import", Code: "import_error", Problem: "Błąd importu XML", ImportID: f.ImportID, File: f.Filename, Details: f.LastError})
		}
	}
	for idx := range out.Rows {
		r := &out.Rows[idx]
		r.ProductURL, r.EditURL = ProductLinks(baseURL, r.WooID)
	}
	sort.SliceStable(out.Rows, func(a, b int) bool {
		x, y := out.Rows[a], out.Rows[b]
		if x.Category != y.Category {
			return x.Category < y.Category
		}
		if x.Code != y.Code {
			return x.Code < y.Code
		}
		if x.PCMID != y.PCMID {
			return x.PCMID < y.PCMID
		}
		return x.WooID < y.WooID
	})
	return out, nil
}

// Links use WordPress ID URLs, so product slugs are not required. Strip any
// credentials/query parameters accidentally included in the configured URL.
func ProductLinks(base string, id uint) (product, edit string) {
	if id == 0 {
		return "", ""
	}
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", ""
	}
	u.User, u.RawQuery, u.Fragment, u.RawFragment = nil, "", "", ""
	u.Path = strings.TrimRight(u.Path, "/") + "/"
	u.RawPath = ""
	q := url.Values{"post_type": {"product"}, "p": {strconv.FormatUint(uint64(id), 10)}}
	u.RawQuery = q.Encode()
	product = u.String()
	u.Path += "wp-admin/post.php"
	u.RawQuery = url.Values{"post": {strconv.FormatUint(uint64(id), 10)}, "action": {"edit"}}.Encode()
	return product, u.String()
}

func Filter(rows []Row, category, query string) []Row {
	query = strings.ToLower(strings.TrimSpace(query))
	var out []Row
	for _, r := range rows {
		if category != "" && r.Category != category {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(fmt.Sprintf("%s %s %s %s %s %s %s %s %d %d %d %d", r.Problem, r.Details, r.PCMName, r.PCMCode, r.WooName, r.WooEAN, r.Operation, r.File, r.PCMID, r.WooID, r.TaskID, r.ImportID)), query) {
			continue
		}
		out = append(out, r)
	}
	return out
}
