package xlstowary

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Row struct {
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

func (r Row) DetailPriceNet() float64 {
	return GrossToNet(r.DetailPriceGross, r.VATRate)
}

func (r Row) WholesaleNet() float64 {
	return GrossToNet(r.WholesaleGross, r.VATRate)
}

type worksheet struct {
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

func Load(xlsxPath string) ([]Row, error) {
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

	var ws worksheet
	if err := xml.Unmarshal(sheetData, &ws); err != nil {
		return nil, err
	}

	var header map[string]int
	rows := make([]Row, 0, len(ws.Rows))
	for _, rawRow := range ws.Rows {
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

		id := ParseInt64(values[header["Id"]])
		if id == 0 {
			continue
		}

		rows = append(rows, Row{
			TowarID:          id,
			Name:             cellValue(values, header, "Nazwa"),
			CashName:         cellValue(values, header, "Na kasie"),
			Code:             cellValue(values, header, "Kod"),
			PurchasePriceNet: ParseFloat(values[header["Cena ew."]]),
			DetailPriceGross: ParseFloat(values[header["Cena det."]]),
			WholesaleGross:   ParseFloat(values[header["Cena hurt."]]),
			VATRate:          NormalizeVAT(ParseFloat(values[header["VAT %"]])),
			Quantity:         ParseFloat(values[header["Ilość"]]),
			Category:         cellValue(values, header, "Kategoria"),
			Producer:         cellValue(values, header, "Producent"),
			UpdatedAt:        ParseExcelDate(values[header["Ost. zmiana"]]),
		})
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].TowarID < rows[j].TowarID })
	return rows, nil
}

func ParseFloat(raw string) float64 {
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

func ParseInt64(raw string) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		floatValue := ParseFloat(raw)
		return int64(floatValue)
	}
	return value
}

func NormalizeVAT(v float64) float64 {
	if v > 1 {
		return RoundMoney(v / 100)
	}
	return RoundMoney(v)
}

func GrossToNet(gross, vatRate float64) float64 {
	if gross == 0 {
		return 0
	}
	multiplier := 1 + NormalizeVAT(vatRate)
	if multiplier == 0 {
		return 0
	}
	return RoundMoney(gross / multiplier)
}

func ParseExcelDate(raw string) string {
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

func EqualFoldTrim(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func SameMoney(a, b float64) bool {
	return RoundMoney(a) == RoundMoney(b)
}

func RoundMoney(v float64) float64 {
	return mathRound(v*100) / 100
}

func FormatMoney(v float64) string {
	return strconv.FormatFloat(RoundMoney(v), 'f', 2, 64)
}

func YesNo(ok bool) string {
	if ok {
		return "TAK"
	}
	return "NIE"
}

func mathRound(v float64) float64 {
	if v < 0 {
		return float64(int64(v - 0.5))
	}
	return float64(int64(v + 0.5))
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
