package problems

import (
	"encoding/csv"
	"io"
	"strconv"
	"strings"
	"time"
)

// WriteCSV exports the displayed snapshot, with UTF-8 BOM and semicolons for
// Polish Excel. Import EAN columns as text to preserve leading zeroes.
func WriteCSV(dst io.Writer, at time.Time, rows []Row) error {
	if _, err := io.WriteString(dst, "\xef\xbb\xbf"); err != nil {
		return err
	}
	w := csv.NewWriter(dst)
	w.Comma, w.UseCRLF = ';', true
	if err := w.Write([]string{"Stan na", "Kategoria", "Kod problemu", "Problem", "Produkt PCM", "ID PCM", "Kod PCM", "Produkt Woo", "ID Woo", "EAN Woo", "Link do produktu", "Edycja w Woo", "Operacja", "ID zadania", "ID importu", "Plik XML", "Szczegóły"}); err != nil {
		return err
	}
	for _, r := range rows {
		fields := []string{at.Format(time.RFC3339), r.Category, r.Code, r.Problem, r.PCMName, idString(r.PCMID), r.PCMCode, r.WooName, idString(int64(r.WooID)), r.WooEAN, r.ProductURL, r.EditURL, r.Operation, idString(int64(r.TaskID)), idString(int64(r.ImportID)), r.File, r.Details}
		for i, field := range fields {
			fields[i] = spreadsheetText(field)
		}
		if err := w.Write(fields); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

func idString(id int64) string {
	if id == 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}

func spreadsheetText(s string) string {
	t := strings.TrimLeft(s, " \t\r\n")
	if t != "" && strings.ContainsRune("=+-@", rune(t[0])) {
		return "'" + s
	}
	return s
}
