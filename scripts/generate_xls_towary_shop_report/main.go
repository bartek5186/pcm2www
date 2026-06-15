package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bartek5186/pcm2www/internal/xlstowary"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type shopProduct struct {
	WooID        uint    `gorm:"column:woo_id"`
	ShopSKU      string  `gorm:"column:shop_sku"`
	ShopEAN      string  `gorm:"column:shop_ean"`
	ShopName     string  `gorm:"column:shop_name"`
	ShopStatus   string  `gorm:"column:shop_status"`
	ShopType     string  `gorm:"column:shop_type"`
	StockQty     float64 `gorm:"column:stock_qty"`
	PriceRegular float64 `gorm:"column:price_regular"`
	PriceSale    float64 `gorm:"column:price_sale"`
	HurtPrice    float64 `gorm:"column:hurt_price"`
	DateModified string  `gorm:"column:date_modified"`
}

type comparisonRow struct {
	Status              string
	Code                string
	XLSTowarIDs         string
	XLSNames            string
	ShopWooIDs          string
	ShopSKUs            string
	ShopNames           string
	NameMatch           string
	XLSDetailPriceGross string
	ShopRegularPrice    string
	RegularPriceMatch   string
	ShopSalePrice       string
	ShopEffectivePrice  string
	EffectivePriceMatch string
	XLSWholesaleNet     string
	ShopHurtPrice       string
	WholesalePriceMatch string
	XLSQuantity         string
	ShopStockQty        string
	StockMatch          string
	ShopStatus          string
	ShopType            string
	Category            string
	Producer            string
	XLSUpdatedAt        string
	ShopDateModified    string
	Note                string
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

	xlsRows, err := xlstowary.Load(*xlsxPath)
	if err != nil {
		fatalf("load xlsx %s: %v", *xlsxPath, err)
	}

	gdb, err := gorm.Open(sqlite.Open(*dbPath), &gorm.Config{})
	if err != nil {
		fatalf("open db %s: %v", *dbPath, err)
	}

	shopRows, err := loadShopProducts(gdb)
	if err != nil {
		fatalf("load shop products: %v", err)
	}

	rows := buildComparisons(xlsRows, shopRows)
	differences := filterDifferences(rows)

	if err := writeComparisonRows(filepath.Join(*outDir, "xls_towary_vs_shop.csv"), rows); err != nil {
		fatalf("write xls vs shop report: %v", err)
	}
	if err := writeComparisonRows(filepath.Join(*outDir, "xls_towary_shop_differences.csv"), differences); err != nil {
		fatalf("write xls vs shop differences report: %v", err)
	}

	statusCounts := map[string]int{}
	for _, row := range rows {
		statusCounts[row.Status]++
	}

	fmt.Printf("Generated %d xls rows, %d shop rows, %d comparison rows, %d difference rows at %s\n",
		len(xlsRows), len(shopRows), len(rows), len(differences), time.Now().Format(time.RFC3339))
	keys := make([]string, 0, len(statusCounts))
	for key := range statusCounts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Printf("%s=%d\n", key, statusCounts[key])
	}
}

func loadShopProducts(gdb *gorm.DB) ([]shopProduct, error) {
	const q = `
SELECT
	woo_id,
	kod AS shop_sku,
	ean AS shop_ean,
	name AS shop_name,
	status AS shop_status,
	type AS shop_type,
	stock_qty,
	price_regular,
	price_sale,
	hurt_price,
	date_modified
FROM woo_product_caches
ORDER BY woo_id;
`
	var rows []shopProduct
	return rows, gdb.Raw(q).Scan(&rows).Error
}

func buildComparisons(xlsRows []xlstowary.Row, shopRows []shopProduct) []comparisonRow {
	xlsByCode := make(map[string][]xlstowary.Row)
	shopByEAN := make(map[string][]shopProduct)
	xlsMissingCode := make([]xlstowary.Row, 0)
	shopMissingEAN := make([]shopProduct, 0)

	for _, row := range xlsRows {
		code := strings.TrimSpace(row.Code)
		if code == "" {
			xlsMissingCode = append(xlsMissingCode, row)
			continue
		}
		xlsByCode[code] = append(xlsByCode[code], row)
	}

	for _, row := range shopRows {
		ean := strings.TrimSpace(row.ShopEAN)
		if ean == "" {
			shopMissingEAN = append(shopMissingEAN, row)
			continue
		}
		shopByEAN[ean] = append(shopByEAN[ean], row)
	}

	rows := make([]comparisonRow, 0, len(xlsRows)+len(shopRows))

	for _, row := range xlsMissingCode {
		rows = append(rows, comparisonRow{
			Status:              "XLS_MISSING_CODE",
			XLSTowarIDs:         strconv.FormatInt(row.TowarID, 10),
			XLSNames:            row.Name,
			XLSDetailPriceGross: xlstowary.FormatMoney(row.DetailPriceGross),
			XLSWholesaleNet:     xlstowary.FormatMoney(row.WholesaleNet()),
			XLSQuantity:         xlstowary.FormatMoney(row.Quantity),
			Category:            row.Category,
			Producer:            row.Producer,
			XLSUpdatedAt:        row.UpdatedAt,
			Note:                "Produkt w XLS nie ma kodu/EAN, więc nie da się go porównać ze sklepem po EAN",
		})
	}

	for _, row := range shopMissingEAN {
		rows = append(rows, comparisonRow{
			Status:             "SHOP_MISSING_EAN",
			ShopWooIDs:         strconv.FormatUint(uint64(row.WooID), 10),
			ShopSKUs:           row.ShopSKU,
			ShopNames:          row.ShopName,
			ShopRegularPrice:   xlstowary.FormatMoney(row.PriceRegular),
			ShopSalePrice:      xlstowary.FormatMoney(row.PriceSale),
			ShopEffectivePrice: xlstowary.FormatMoney(shopEffectivePrice(row)),
			ShopHurtPrice:      xlstowary.FormatMoney(row.HurtPrice),
			ShopStockQty:       xlstowary.FormatMoney(row.StockQty),
			ShopStatus:         row.ShopStatus,
			ShopType:           row.ShopType,
			ShopDateModified:   row.DateModified,
			Note:               "Produkt w sklepie nie ma EAN, więc nie da się go porównać z XLS po EAN",
		})
	}

	keys := make(map[string]struct{}, len(xlsByCode)+len(shopByEAN))
	for code := range xlsByCode {
		keys[code] = struct{}{}
	}
	for ean := range shopByEAN {
		keys[ean] = struct{}{}
	}

	sortedKeys := make([]string, 0, len(keys))
	for key := range keys {
		sortedKeys = append(sortedKeys, key)
	}
	sort.Strings(sortedKeys)

	for _, key := range sortedKeys {
		xList := xlsByCode[key]
		sList := shopByEAN[key]

		row := comparisonRow{
			Code:                key,
			XLSTowarIDs:         joinXLSTowarIDs(xList),
			XLSNames:            joinXLSNames(xList),
			ShopWooIDs:          joinShopWooIDs(sList),
			ShopSKUs:            joinShopSKUs(sList),
			ShopNames:           joinShopNames(sList),
			XLSDetailPriceGross: formatXLSDetail(xList),
			XLSWholesaleNet:     formatXLSWholesale(xList),
			XLSQuantity:         formatXLSQuantity(xList),
			ShopRegularPrice:    formatShopRegular(sList),
			ShopSalePrice:       formatShopSale(sList),
			ShopEffectivePrice:  formatShopEffective(sList),
			ShopHurtPrice:       formatShopHurt(sList),
			ShopStockQty:        formatShopStock(sList),
			ShopStatus:          joinShopStatuses(sList),
			ShopType:            joinShopTypes(sList),
			Category:            joinXLSCategories(xList),
			Producer:            joinXLSProducers(xList),
			XLSUpdatedAt:        joinXLSUpdatedAt(xList),
			ShopDateModified:    joinShopDateModified(sList),
		}

		switch {
		case len(xList) > 1:
			row.Status = "DUPLICATE_XLS_CODE"
			row.Note = fmt.Sprintf("Kod/EAN %s występuje wielokrotnie w XLS (%d razy)", key, len(xList))
		case len(sList) > 1:
			row.Status = "DUPLICATE_SHOP_EAN"
			row.Note = fmt.Sprintf("EAN %s występuje wielokrotnie w sklepie (%d razy)", key, len(sList))
		case len(xList) == 1 && len(sList) == 0:
			row.Status = "ONLY_XLS"
			row.Note = "Produkt jest w XLS, ale nie ma go w sklepie po EAN"
		case len(xList) == 0 && len(sList) == 1:
			row.Status = "ONLY_SHOP"
			row.Note = "Produkt jest w sklepie, ale nie ma go w XLS po EAN"
		case len(xList) == 1 && len(sList) == 1:
			x := xList[0]
			s := sList[0]
			row.NameMatch = xlstowary.YesNo(xlstowary.EqualFoldTrim(x.Name, s.ShopName))
			row.RegularPriceMatch = xlstowary.YesNo(xlstowary.SameMoney(x.DetailPriceGross, s.PriceRegular))
			row.EffectivePriceMatch = xlstowary.YesNo(xlstowary.SameMoney(x.DetailPriceGross, shopEffectivePrice(s)))
			row.WholesalePriceMatch = xlstowary.YesNo(xlstowary.SameMoney(x.WholesaleNet(), s.HurtPrice))
			row.StockMatch = xlstowary.YesNo(xlstowary.SameMoney(x.Quantity, s.StockQty))

			var diffs []string
			if row.NameMatch == "NIE" {
				diffs = append(diffs, "name")
			}
			if row.RegularPriceMatch == "NIE" {
				diffs = append(diffs, "regular_price")
			}
			if row.EffectivePriceMatch == "NIE" {
				diffs = append(diffs, "effective_price")
			}
			if row.WholesalePriceMatch == "NIE" {
				diffs = append(diffs, "hurt_price")
			}
			if row.StockMatch == "NIE" {
				diffs = append(diffs, "stock")
			}
			if s.PriceSale > 0 {
				diffs = appendUnique(diffs, "sale_active")
			}

			if len(diffs) == 0 {
				row.Status = "MATCH"
				row.Note = "Pełna zgodność XLS -> sklep po EAN"
			} else {
				row.Status = "DIFF"
				row.Note = "Różnice: " + strings.Join(diffs, ", ")
			}
		default:
			row.Status = "UNCLASSIFIED"
			row.Note = "Nietypowy układ danych po EAN"
		}

		rows = append(rows, row)
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Status != rows[j].Status {
			return rows[i].Status < rows[j].Status
		}
		if rows[i].Code != rows[j].Code {
			return rows[i].Code < rows[j].Code
		}
		if rows[i].XLSTowarIDs != rows[j].XLSTowarIDs {
			return rows[i].XLSTowarIDs < rows[j].XLSTowarIDs
		}
		return rows[i].ShopWooIDs < rows[j].ShopWooIDs
	})

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
		"code",
		"xls_towar_ids",
		"xls_names",
		"shop_woo_ids",
		"shop_skus",
		"shop_names",
		"name_match",
		"xls_detail_price_gross",
		"shop_regular_price",
		"regular_price_match",
		"shop_sale_price",
		"shop_effective_price",
		"effective_price_match",
		"xls_wholesale_net",
		"shop_hurt_price",
		"wholesale_price_match",
		"xls_quantity",
		"shop_stock_qty",
		"stock_match",
		"shop_status",
		"shop_type",
		"category",
		"producer",
		"xls_updated_at",
		"shop_date_modified",
		"note",
	}
	if err := w.Write(header); err != nil {
		return err
	}

	for _, row := range rows {
		record := []string{
			row.Status,
			row.Code,
			row.XLSTowarIDs,
			row.XLSNames,
			row.ShopWooIDs,
			row.ShopSKUs,
			row.ShopNames,
			row.NameMatch,
			row.XLSDetailPriceGross,
			row.ShopRegularPrice,
			row.RegularPriceMatch,
			row.ShopSalePrice,
			row.ShopEffectivePrice,
			row.EffectivePriceMatch,
			row.XLSWholesaleNet,
			row.ShopHurtPrice,
			row.WholesalePriceMatch,
			row.XLSQuantity,
			row.ShopStockQty,
			row.StockMatch,
			row.ShopStatus,
			row.ShopType,
			row.Category,
			row.Producer,
			row.XLSUpdatedAt,
			row.ShopDateModified,
			row.Note,
		}
		if err := w.Write(record); err != nil {
			return err
		}
	}
	return w.Error()
}

func shopEffectivePrice(row shopProduct) float64 {
	if row.PriceSale > 0 {
		return row.PriceSale
	}
	return row.PriceRegular
}

func appendUnique(items []string, value string) []string {
	for _, item := range items {
		if item == value {
			return items
		}
	}
	return append(items, value)
}

func joinXLSTowarIDs(rows []xlstowary.Row) string {
	values := make([]string, 0, len(rows))
	for _, row := range rows {
		values = append(values, strconv.FormatInt(row.TowarID, 10))
	}
	return strings.Join(values, "|")
}

func joinXLSNames(rows []xlstowary.Row) string {
	values := make([]string, 0, len(rows))
	for _, row := range rows {
		values = append(values, row.Name)
	}
	return strings.Join(values, " | ")
}

func joinShopWooIDs(rows []shopProduct) string {
	values := make([]string, 0, len(rows))
	for _, row := range rows {
		values = append(values, strconv.FormatUint(uint64(row.WooID), 10))
	}
	return strings.Join(values, "|")
}

func joinShopSKUs(rows []shopProduct) string {
	values := make([]string, 0, len(rows))
	for _, row := range rows {
		values = append(values, row.ShopSKU)
	}
	return strings.Join(values, " | ")
}

func joinShopNames(rows []shopProduct) string {
	values := make([]string, 0, len(rows))
	for _, row := range rows {
		values = append(values, row.ShopName)
	}
	return strings.Join(values, " | ")
}

func formatXLSDetail(rows []xlstowary.Row) string {
	values := make([]string, 0, len(rows))
	for _, row := range rows {
		values = append(values, xlstowary.FormatMoney(row.DetailPriceGross))
	}
	return strings.Join(values, "|")
}

func formatXLSWholesale(rows []xlstowary.Row) string {
	values := make([]string, 0, len(rows))
	for _, row := range rows {
		values = append(values, xlstowary.FormatMoney(row.WholesaleNet()))
	}
	return strings.Join(values, "|")
}

func formatXLSQuantity(rows []xlstowary.Row) string {
	values := make([]string, 0, len(rows))
	for _, row := range rows {
		values = append(values, xlstowary.FormatMoney(row.Quantity))
	}
	return strings.Join(values, "|")
}

func formatShopRegular(rows []shopProduct) string {
	values := make([]string, 0, len(rows))
	for _, row := range rows {
		values = append(values, xlstowary.FormatMoney(row.PriceRegular))
	}
	return strings.Join(values, "|")
}

func formatShopSale(rows []shopProduct) string {
	values := make([]string, 0, len(rows))
	for _, row := range rows {
		values = append(values, xlstowary.FormatMoney(row.PriceSale))
	}
	return strings.Join(values, "|")
}

func formatShopEffective(rows []shopProduct) string {
	values := make([]string, 0, len(rows))
	for _, row := range rows {
		values = append(values, xlstowary.FormatMoney(shopEffectivePrice(row)))
	}
	return strings.Join(values, "|")
}

func formatShopHurt(rows []shopProduct) string {
	values := make([]string, 0, len(rows))
	for _, row := range rows {
		values = append(values, xlstowary.FormatMoney(row.HurtPrice))
	}
	return strings.Join(values, "|")
}

func formatShopStock(rows []shopProduct) string {
	values := make([]string, 0, len(rows))
	for _, row := range rows {
		values = append(values, xlstowary.FormatMoney(row.StockQty))
	}
	return strings.Join(values, "|")
}

func joinShopStatuses(rows []shopProduct) string {
	values := make([]string, 0, len(rows))
	for _, row := range rows {
		values = append(values, row.ShopStatus)
	}
	return strings.Join(values, "|")
}

func joinShopTypes(rows []shopProduct) string {
	values := make([]string, 0, len(rows))
	for _, row := range rows {
		values = append(values, row.ShopType)
	}
	return strings.Join(values, "|")
}

func joinXLSCategories(rows []xlstowary.Row) string {
	values := make([]string, 0, len(rows))
	for _, row := range rows {
		if strings.TrimSpace(row.Category) == "" {
			continue
		}
		values = append(values, row.Category)
	}
	return strings.Join(values, " | ")
}

func joinXLSProducers(rows []xlstowary.Row) string {
	values := make([]string, 0, len(rows))
	for _, row := range rows {
		if strings.TrimSpace(row.Producer) == "" {
			continue
		}
		values = append(values, row.Producer)
	}
	return strings.Join(values, " | ")
}

func joinXLSUpdatedAt(rows []xlstowary.Row) string {
	values := make([]string, 0, len(rows))
	for _, row := range rows {
		if strings.TrimSpace(row.UpdatedAt) == "" {
			continue
		}
		values = append(values, row.UpdatedAt)
	}
	return strings.Join(values, " | ")
}

func joinShopDateModified(rows []shopProduct) string {
	values := make([]string, 0, len(rows))
	for _, row := range rows {
		if strings.TrimSpace(row.DateModified) == "" {
			continue
		}
		values = append(values, row.DateModified)
	}
	return strings.Join(values, " | ")
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
