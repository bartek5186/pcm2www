package main

import (
	"archive/zip"
	"encoding/csv"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type xlsTowaryRow struct {
	TowarID          int64
	Name             string
	CashName         string
	Code             string
	PurchasePriceNet float64
	DetailPriceGross float64
	WholesaleGross   float64
	VATRate          float64
	Quantity         float64
	Category         string
	Producer         string
	UpdatedAt        string
}

func (r xlsTowaryRow) DetailPriceNet() float64 {
	return grossToNet(r.DetailPriceGross, r.VATRate)
}

func (r xlsTowaryRow) WholesaleNet() float64 {
	return grossToNet(r.WholesaleGross, r.VATRate)
}

type magazineRow struct {
	TowarID       int64   `gorm:"column:towar_id"`
	MagEAN        string  `gorm:"column:mag_ean"`
	MagName       string  `gorm:"column:mag_name"`
	CenaDetal     float64 `gorm:"column:cena_detal"`
	CenaHurtowa   float64 `gorm:"column:cena_hurtowa"`
	TotalStock    float64 `gorm:"column:total_stock"`
	TotalReserved float64 `gorm:"column:total_reserved"`
	ImportID      uint    `gorm:"column:import_id"`
	UpdatedAt     string  `gorm:"column:updated_at"`
}

type comparisonRow struct {
	Status               string
	TowarID              int64
	XLSName              string
	MagazineName         string
	NameMatch            string
	XLSCode              string
	MagazineCode         string
	CodeMatch            string
	XLSVATRate           float64
	XLSPurchasePriceNet  float64
	XLSDetailPriceGross  float64
	XLSDetailPriceNet    float64
	MagazineDetailNet    float64
	DetailPriceMatch     string
	XLSWholesaleGross    float64
	XLSWholesaleNet      float64
	MagazineWholesaleNet float64
	WholesalePriceMatch  string
	XLSQuantity          float64
	MagazineQuantity     float64
	StockMatch           string
	Category             string
	Producer             string
	XLSUpdatedAt         string
	MagazineUpdatedAt    string
	Note                 string
}

type xlsxWorksheet struct {
	Rows []xlsxRow `xml:"sheetData>row"`
}

type xlsxRow struct {
	Cells []xlsxCell `xml:"c"`
}

type xlsxCell struct {
	Ref    string      `xml:"r,attr"`
	Type   string      `xml:"t,attr"`
	Value  string      `xml:"v"`
	Inline *xlsxInline `xml:"is"`
}

type xlsxInline struct {
	Text string        `xml:"t"`
	Runs []xlsxTextRun `xml:"r"`
}

type xlsxTextRun struct {
	Text string `xml:"t"`
}

type sharedStringTable struct {
	Items []sharedStringItem `xml:"si"`
}

type sharedStringItem struct {
	Text string        `xml:"t"`
	Runs []xlsxTextRun `xml:"r"`
}

func main() {
	home, _ := os.UserHomeDir()
	defaultDB := filepath.Join(home, ".config", "pcm2www", "pcm2www.db")

	dbPath := flag.String("db", defaultDB, "path to pcm2www sqlite database")
	xlsxPath := flag.String("xlsx", "XLS_Towary.xlsx", "path to XLS_Towary.xlsx export")
	outDir := flag.String("out", "reports", "directory for generated csv reports")
	flag.Parse()

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fatalf("mkdir %s: %v", *outDir, err)
	}

	xlsRows, err := loadXLSTowary(*xlsxPath)
	if err != nil {
		fatalf("load xlsx %s: %v", *xlsxPath, err)
	}

	gdb, err := gorm.Open(sqlite.Open(*dbPath), &gorm.Config{})
	if err != nil {
		fatalf("open db %s: %v", *dbPath, err)
	}

	magRows, err := loadMagazineProducts(gdb)
	if err != nil {
		fatalf("load magazine products: %v", err)
	}

	comparisons := buildComparisons(xlsRows, magRows)
	differences := filterDifferences(comparisons)

	if err := writeXLSTowaryRows(filepath.Join(*outDir, "xls_towary_products.csv"), xlsRows); err != nil {
		fatalf("write xls products report: %v", err)
	}
	if err := writeComparisonRows(filepath.Join(*outDir, "xls_towary_vs_magazine.csv"), comparisons); err != nil {
		fatalf("write xls vs magazine report: %v", err)
	}
	if err := writeComparisonRows(filepath.Join(*outDir, "xls_towary_differences.csv"), differences); err != nil {
		fatalf("write xls differences report: %v", err)
	}

	statusCounts := map[string]int{}
	for _, row := range comparisons {
		statusCounts[row.Status]++
	}

	fmt.Printf("Generated %d xls rows, %d magazine rows, %d comparison rows, %d difference rows at %s\n",
		len(xlsRows), len(magRows), len(comparisons), len(differences), time.Now().Format(time.RFC3339))
	for _, key := range []string{"MATCH", "DIFF", "ONLY_XLS", "ONLY_MAGAZINE"} {
		fmt.Printf("%s=%d\n", key, statusCounts[key])
	}
}

func loadXLSTowary(xlsxPath string) ([]xlsTowaryRow, error) {
	zr, err := zip.OpenReader(xlsxPath)
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	sharedStrings, err := readSharedStrings(&zr.Reader)
	if err != nil {
		return nil, err
	}

	sheetPath, err := firstWorksheetPath(&zr.Reader)
	if err != nil {
		return nil, err
	}

	sheetData, err := readZipFile(&zr.Reader, sheetPath)
	if err != nil {
		return nil, err
	}

	var worksheet xlsxWorksheet
	if err := xml.Unmarshal(sheetData, &worksheet); err != nil {
		return nil, err
	}

	var header map[string]int
	rows := make([]xlsTowaryRow, 0, len(worksheet.Rows))
	for _, rawRow := range worksheet.Rows {
		values := decodeWorksheetRow(rawRow, sharedStrings)
		if len(values) == 0 {
			continue
		}
		if header == nil {
			candidate := make(map[string]int)
			for col, value := range values {
				if value == "" {
					continue
				}
				candidate[value] = col
			}
			if _, ok := candidate["Id"]; ok {
				if _, ok := candidate["Nazwa"]; ok {
					if _, ok := candidate["Kod"]; ok {
						header = candidate
					}
				}
			}
			continue
		}

		id := parseInt64(values[header["Id"]])
		if id == 0 {
			continue
		}

		row := xlsTowaryRow{
			TowarID:          id,
			Name:             cellValue(values, header, "Nazwa"),
			CashName:         cellValue(values, header, "Na kasie"),
			Code:             cellValue(values, header, "Kod"),
			PurchasePriceNet: parseFloat(values[header["Cena ew."]]),
			DetailPriceGross: parseFloat(values[header["Cena det."]]),
			WholesaleGross:   parseFloat(values[header["Cena hurt."]]),
			VATRate:          normalizeVAT(parseFloat(values[header["VAT %"]])),
			Quantity:         parseFloat(values[header["Ilość"]]),
			Category:         cellValue(values, header, "Kategoria"),
			Producer:         cellValue(values, header, "Producent"),
			UpdatedAt:        parseExcelDate(values[header["Ost. zmiana"]]),
		}
		rows = append(rows, row)
	}

	sort.Slice(rows, func(i, j int) bool {
		return rows[i].TowarID < rows[j].TowarID
	})
	return rows, nil
}

func loadMagazineProducts(gdb *gorm.DB) ([]magazineRow, error) {
	const q = `
SELECT
	p.towar_id,
	p.kod AS mag_ean,
	p.nazwa AS mag_name,
	p.cena_detal,
	p.cena_hurtowa,
	COALESCE(SUM(s.stan), 0) AS total_stock,
	COALESCE(SUM(s.rezerwacja), 0) AS total_reserved,
	p.import_id,
	COALESCE(CAST(p.updated_at AS TEXT), '') AS updated_at
FROM st_products p
LEFT JOIN st_stocks s ON s.towar_id = p.towar_id
GROUP BY
	p.towar_id,
	p.kod,
	p.nazwa,
	p.cena_detal,
	p.cena_hurtowa,
	p.import_id,
	p.updated_at
ORDER BY p.towar_id;
`
	var rows []magazineRow
	return rows, gdb.Raw(q).Scan(&rows).Error
}

func buildComparisons(xlsRows []xlsTowaryRow, magRows []magazineRow) []comparisonRow {
	xlsByID := make(map[int64]xlsTowaryRow, len(xlsRows))
	magByID := make(map[int64]magazineRow, len(magRows))
	ids := make(map[int64]struct{}, len(xlsRows)+len(magRows))

	for _, row := range xlsRows {
		xlsByID[row.TowarID] = row
		ids[row.TowarID] = struct{}{}
	}
	for _, row := range magRows {
		magByID[row.TowarID] = row
		ids[row.TowarID] = struct{}{}
	}

	sortedIDs := make([]int64, 0, len(ids))
	for id := range ids {
		sortedIDs = append(sortedIDs, id)
	}
	sort.Slice(sortedIDs, func(i, j int) bool { return sortedIDs[i] < sortedIDs[j] })

	rows := make([]comparisonRow, 0, len(sortedIDs))
	for _, id := range sortedIDs {
		xls, hasXLS := xlsByID[id]
		mag, hasMag := magByID[id]

		row := comparisonRow{
			TowarID:              id,
			XLSName:              xls.Name,
			MagazineName:         mag.MagName,
			XLSCode:              xls.Code,
			MagazineCode:         mag.MagEAN,
			XLSVATRate:           xls.VATRate,
			XLSPurchasePriceNet:  roundMoney(xls.PurchasePriceNet),
			XLSDetailPriceGross:  roundMoney(xls.DetailPriceGross),
			XLSDetailPriceNet:    roundMoney(xls.DetailPriceNet()),
			MagazineDetailNet:    roundMoney(mag.CenaDetal),
			XLSWholesaleGross:    roundMoney(xls.WholesaleGross),
			XLSWholesaleNet:      roundMoney(xls.WholesaleNet()),
			MagazineWholesaleNet: roundMoney(mag.CenaHurtowa),
			XLSQuantity:          roundMoney(xls.Quantity),
			MagazineQuantity:     roundMoney(mag.TotalStock),
			Category:             xls.Category,
			Producer:             xls.Producer,
			XLSUpdatedAt:         xls.UpdatedAt,
			MagazineUpdatedAt:    mag.UpdatedAt,
		}

		switch {
		case hasXLS && !hasMag:
			row.Status = "ONLY_XLS"
			row.Note = "Produkt jest w XLS, ale nie ma go w bieżącym stagingu magazynowym"
		case !hasXLS && hasMag:
			row.Status = "ONLY_MAGAZINE"
			row.Note = "Produkt jest w stagingu magazynowym, ale nie ma go w XLS"
		default:
			row.NameMatch = yesNo(equalFoldTrim(xls.Name, mag.MagName))
			row.CodeMatch = yesNo(equalFoldTrim(xls.Code, mag.MagEAN))
			row.DetailPriceMatch = yesNo(sameMoney(xls.DetailPriceNet(), mag.CenaDetal))
			row.WholesalePriceMatch = yesNo(sameMoney(xls.WholesaleNet(), mag.CenaHurtowa))
			row.StockMatch = yesNo(sameMoney(xls.Quantity, mag.TotalStock))

			var mismatches []string
			if row.NameMatch == "NIE" {
				mismatches = append(mismatches, "name")
			}
			if row.CodeMatch == "NIE" {
				mismatches = append(mismatches, "code")
			}
			if row.DetailPriceMatch == "NIE" {
				mismatches = append(mismatches, "detail_price")
			}
			if row.WholesalePriceMatch == "NIE" {
				mismatches = append(mismatches, "wholesale_price")
			}
			if row.StockMatch == "NIE" {
				mismatches = append(mismatches, "stock")
			}

			if len(mismatches) == 0 {
				row.Status = "MATCH"
				row.Note = "Pełna zgodność po towar_id"
			} else {
				row.Status = "DIFF"
				row.Note = "Różnice: " + strings.Join(mismatches, ", ")
			}
		}

		rows = append(rows, row)
	}

	return rows
}

func filterDifferences(rows []comparisonRow) []comparisonRow {
	filtered := make([]comparisonRow, 0, len(rows))
	for _, row := range rows {
		if row.Status != "MATCH" {
			filtered = append(filtered, row)
		}
	}
	return filtered
}

func writeXLSTowaryRows(path string, rows []xlsTowaryRow) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	w := csv.NewWriter(file)
	defer w.Flush()

	header := []string{
		"towar_id",
		"name",
		"cash_name",
		"code",
		"purchase_price_net",
		"detail_price_gross",
		"detail_price_net",
		"wholesale_gross",
		"wholesale_net",
		"vat_rate",
		"quantity",
		"category",
		"producer",
		"updated_at",
	}
	if err := w.Write(header); err != nil {
		return err
	}

	for _, row := range rows {
		record := []string{
			strconv.FormatInt(row.TowarID, 10),
			row.Name,
			row.CashName,
			row.Code,
			formatMoney(row.PurchasePriceNet),
			formatMoney(row.DetailPriceGross),
			formatMoney(row.DetailPriceNet()),
			formatMoney(row.WholesaleGross),
			formatMoney(row.WholesaleNet()),
			formatMoney(row.VATRate),
			formatMoney(row.Quantity),
			row.Category,
			row.Producer,
			row.UpdatedAt,
		}
		if err := w.Write(record); err != nil {
			return err
		}
	}
	return w.Error()
}

func writeComparisonRows(path string, rows []comparisonRow) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	w := csv.NewWriter(file)
	defer w.Flush()

	header := []string{
		"status",
		"towar_id",
		"xls_name",
		"magazine_name",
		"name_match",
		"xls_code",
		"magazine_code",
		"code_match",
		"xls_vat_rate",
		"xls_purchase_price_net",
		"xls_detail_price_gross",
		"xls_detail_price_net",
		"magazine_detail_price_net",
		"detail_price_match",
		"xls_wholesale_gross",
		"xls_wholesale_net",
		"magazine_wholesale_net",
		"wholesale_price_match",
		"xls_quantity",
		"magazine_quantity",
		"stock_match",
		"category",
		"producer",
		"xls_updated_at",
		"magazine_updated_at",
		"note",
	}
	if err := w.Write(header); err != nil {
		return err
	}

	for _, row := range rows {
		record := []string{
			row.Status,
			strconv.FormatInt(row.TowarID, 10),
			row.XLSName,
			row.MagazineName,
			row.NameMatch,
			row.XLSCode,
			row.MagazineCode,
			row.CodeMatch,
			formatMoney(row.XLSVATRate),
			formatMoney(row.XLSPurchasePriceNet),
			formatMoney(row.XLSDetailPriceGross),
			formatMoney(row.XLSDetailPriceNet),
			formatMoney(row.MagazineDetailNet),
			row.DetailPriceMatch,
			formatMoney(row.XLSWholesaleGross),
			formatMoney(row.XLSWholesaleNet),
			formatMoney(row.MagazineWholesaleNet),
			row.WholesalePriceMatch,
			formatMoney(row.XLSQuantity),
			formatMoney(row.MagazineQuantity),
			row.StockMatch,
			row.Category,
			row.Producer,
			row.XLSUpdatedAt,
			row.MagazineUpdatedAt,
			row.Note,
		}
		if err := w.Write(record); err != nil {
			return err
		}
	}
	return w.Error()
}

func readSharedStrings(zr *zip.Reader) ([]string, error) {
	data, err := readZipFile(zr, "xl/sharedStrings.xml")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var table sharedStringTable
	if err := xml.Unmarshal(data, &table); err != nil {
		return nil, err
	}

	result := make([]string, 0, len(table.Items))
	for _, item := range table.Items {
		result = append(result, joinRichText(item.Text, item.Runs))
	}
	return result, nil
}

func firstWorksheetPath(zr *zip.Reader) (string, error) {
	paths := make([]string, 0, 4)
	for _, file := range zr.File {
		if strings.HasPrefix(file.Name, "xl/worksheets/") && strings.HasSuffix(file.Name, ".xml") {
			paths = append(paths, file.Name)
		}
	}
	if len(paths) == 0 {
		return "", fmt.Errorf("no worksheets found in xlsx")
	}
	sort.Strings(paths)
	return paths[0], nil
}

func readZipFile(zr *zip.Reader, name string) ([]byte, error) {
	for _, file := range zr.File {
		if file.Name != name {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(rc)
	}
	return nil, os.ErrNotExist
}

func decodeWorksheetRow(row xlsxRow, sharedStrings []string) map[int]string {
	values := make(map[int]string, len(row.Cells))
	for _, cell := range row.Cells {
		col := columnIndex(cell.Ref)
		values[col] = decodeCellValue(cell, sharedStrings)
	}
	return values
}

func decodeCellValue(cell xlsxCell, sharedStrings []string) string {
	switch cell.Type {
	case "s":
		idx, err := strconv.Atoi(strings.TrimSpace(cell.Value))
		if err != nil || idx < 0 || idx >= len(sharedStrings) {
			return ""
		}
		return strings.TrimSpace(sharedStrings[idx])
	case "inlineStr":
		if cell.Inline == nil {
			return ""
		}
		return strings.TrimSpace(joinRichText(cell.Inline.Text, cell.Inline.Runs))
	case "str":
		return strings.TrimSpace(cell.Value)
	default:
		return strings.TrimSpace(cell.Value)
	}
}

func joinRichText(text string, runs []xlsxTextRun) string {
	if len(runs) == 0 {
		return text
	}
	var b strings.Builder
	if text != "" {
		b.WriteString(text)
	}
	for _, run := range runs {
		b.WriteString(run.Text)
	}
	return b.String()
}

func columnIndex(ref string) int {
	letters := make([]rune, 0, 4)
	for _, r := range ref {
		if r >= 'A' && r <= 'Z' {
			letters = append(letters, r)
		}
	}
	if len(letters) == 0 {
		return 0
	}
	col := 0
	for _, r := range letters {
		col = col*26 + int(r-'A'+1)
	}
	return col - 1
}

func cellValue(values map[int]string, header map[string]int, key string) string {
	idx, ok := header[key]
	if !ok {
		return ""
	}
	return strings.TrimSpace(values[idx])
}

func parseFloat(raw string) float64 {
	raw = strings.TrimSpace(strings.ReplaceAll(raw, ",", "."))
	if raw == "" {
		return 0
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0
	}
	return value
}

func parseInt64(raw string) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		floatValue := parseFloat(raw)
		return int64(floatValue)
	}
	return value
}

func normalizeVAT(v float64) float64 {
	if v > 1 {
		return roundMoney(v / 100)
	}
	return roundMoney(v)
}

func grossToNet(gross, vatRate float64) float64 {
	if gross == 0 {
		return 0
	}
	multiplier := 1 + normalizeVAT(vatRate)
	if multiplier == 0 {
		return 0
	}
	return roundMoney(gross / multiplier)
}

func parseExcelDate(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if value, err := strconv.ParseFloat(raw, 64); err == nil && value > 0 {
		base := time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)
		t := base.Add(time.Duration(value*24) * time.Hour)
		return t.Format("2006-01-02 15:04:05")
	}
	return raw
}

func equalFoldTrim(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func sameMoney(a, b float64) bool {
	return roundMoney(a) == roundMoney(b)
}

func roundMoney(v float64) float64 {
	return mathRound(v*100) / 100
}

func mathRound(v float64) float64 {
	if v < 0 {
		return float64(int64(v - 0.5))
	}
	return float64(int64(v + 0.5))
}

func formatMoney(v float64) string {
	return strconv.FormatFloat(roundMoney(v), 'f', 2, 64)
}

func yesNo(ok bool) string {
	if ok {
		return "TAK"
	}
	return "NIE"
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
