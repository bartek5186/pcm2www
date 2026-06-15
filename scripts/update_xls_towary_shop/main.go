package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
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

const batchSize = 20

type runtimeConfig struct {
	Integrations struct {
		WooCommerce struct {
			BaseURL        string `json:"base_url"`
			ConsumerKey    string `json:"consumer_key"`
			ConsumerSecret string `json:"consumer_secret"`
		} `json:"woocommerce"`
	} `json:"integrations"`
}

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

type wcMetaData struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
}

type wcProduct struct {
	ID            int64        `json:"id"`
	Name          string       `json:"name"`
	SKU           string       `json:"sku"`
	RegularPrice  string       `json:"regular_price"`
	SalePrice     string       `json:"sale_price"`
	HurtPrice     string       `json:"hurt_price"`
	TaxClass      string       `json:"tax_class"`
	ManageStock   bool         `json:"manage_stock"`
	StockQuantity float64      `json:"stock_quantity"`
	StockStatus   string       `json:"stock_status"`
	MetaData      []wcMetaData `json:"meta_data"`
}

type batchResponse struct {
	Update []wcProduct `json:"update"`
}

type updatePlan struct {
	WooID               uint
	ShopSKU             string
	ShopEAN             string
	ShopName            string
	XLSTowarID          int64
	XLSName             string
	DesiredRegularGross float64
	DesiredHurtGross    float64
	DesiredTaxClass     string
	DesiredStock        int
	DesiredStockRaw     float64
}

type updateResult struct {
	updatePlan
	Result               string
	Error                string
	VerifiedRegularGross float64
	VerifiedHurtGross    float64
	VerifiedTaxClass     string
	VerifiedStock        float64
	VerifiedSalePrice    float64
	VerifiedStockStatus  string
}

func main() {
	home, _ := os.UserHomeDir()
	defaultDB := filepath.Join(home, ".config", "pcm2www", "pcm2www.db")
	defaultCfg := filepath.Join(home, ".config", "pcm2www", "config.json")

	dbPath := flag.String("db", defaultDB, "path to pcm2www sqlite database")
	configPath := flag.String("config", defaultCfg, "path to app config json")
	xlsxPath := flag.String("xlsx", "XLS_Towary.xlsx", "path to XLS_Towary.xlsx export")
	outDir := flag.String("out", "reports", "directory for generated csv reports")
	limit := flag.Int("limit", 0, "limit number of products to update, 0 means all")
	flag.Parse()

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fatalf("mkdir %s: %v", *outDir, err)
	}

	cfg, err := loadRuntimeConfig(*configPath)
	if err != nil {
		fatalf("load config %s: %v", *configPath, err)
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

	plans := buildUpdatePlans(xlsRows, shopRows)
	if *limit > 0 && *limit < len(plans) {
		plans = plans[:*limit]
	}

	if err := writePlans(filepath.Join(*outDir, "xls_shop_price_stock_tax_updates.csv"), plans); err != nil {
		fatalf("write plans: %v", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	results := applyUpdates(client, cfg, plans)

	if err := writeResults(filepath.Join(*outDir, "xls_shop_price_stock_tax_update_results.csv"), results); err != nil {
		fatalf("write results: %v", err)
	}

	counts := map[string]int{}
	for _, result := range results {
		counts[result.Result]++
	}

	fmt.Printf("Prepared %d updates and wrote results at %s\n", len(plans), time.Now().Format(time.RFC3339))
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Printf("%s=%d\n", key, counts[key])
	}
}

func loadRuntimeConfig(path string) (runtimeConfig, error) {
	var cfg runtimeConfig
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, err
	}
	if strings.TrimSpace(cfg.Integrations.WooCommerce.BaseURL) == "" {
		return cfg, fmt.Errorf("woocommerce.base_url missing")
	}
	if strings.TrimSpace(cfg.Integrations.WooCommerce.ConsumerKey) == "" || strings.TrimSpace(cfg.Integrations.WooCommerce.ConsumerSecret) == "" {
		return cfg, fmt.Errorf("woocommerce credentials missing")
	}
	return cfg, nil
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

func buildUpdatePlans(xlsRows []xlstowary.Row, shopRows []shopProduct) []updatePlan {
	xlsByCode := make(map[string][]xlstowary.Row)
	shopByEAN := make(map[string][]shopProduct)

	for _, row := range xlsRows {
		code := strings.TrimSpace(row.Code)
		if code == "" {
			continue
		}
		xlsByCode[code] = append(xlsByCode[code], row)
	}
	for _, row := range shopRows {
		ean := strings.TrimSpace(row.ShopEAN)
		if ean == "" {
			continue
		}
		shopByEAN[ean] = append(shopByEAN[ean], row)
	}

	plans := make([]updatePlan, 0, 1024)
	for code, xlsList := range xlsByCode {
		if len(xlsList) != 1 {
			continue
		}
		shopList := shopByEAN[code]
		if len(shopList) != 1 {
			continue
		}

		x := xlsList[0]
		s := shopList[0]
		plans = append(plans, updatePlan{
			WooID:               s.WooID,
			ShopSKU:             s.ShopSKU,
			ShopEAN:             s.ShopEAN,
			ShopName:            s.ShopName,
			XLSTowarID:          x.TowarID,
			XLSName:             x.Name,
			DesiredRegularGross: xlstowary.RoundMoney(x.DetailPriceGross),
			DesiredHurtGross:    xlstowary.RoundMoney(x.WholesaleGross),
			DesiredTaxClass:     mapTaxClass(x.VATRate),
			DesiredStock:        normalizeStock(x.Quantity),
			DesiredStockRaw:     xlstowary.RoundMoney(x.Quantity),
		})
	}

	sort.Slice(plans, func(i, j int) bool {
		if plans[i].WooID != plans[j].WooID {
			return plans[i].WooID < plans[j].WooID
		}
		return plans[i].XLSTowarID < plans[j].XLSTowarID
	})
	return plans
}

func mapTaxClass(vatRate float64) string {
	switch xlstowary.RoundMoney(vatRate) {
	case 0.05:
		return "500"
	case 0.07:
		return "800"
	default:
		return "2300"
	}
}

func normalizeStock(v float64) int {
	if v <= 0 {
		return 0
	}
	return int(xlstowary.RoundMoney(v))
}

func applyUpdates(client *http.Client, cfg runtimeConfig, plans []updatePlan) []updateResult {
	results := make([]updateResult, 0, len(plans))
	ctx := context.Background()

	for start := 0; start < len(plans); start += batchSize {
		end := start + batchSize
		if end > len(plans) {
			end = len(plans)
		}
		batch := plans[start:end]

		products, err := updateBatch(ctx, client, cfg, batch)
		if err != nil {
			for _, plan := range batch {
				result, singleErr := updateSingle(ctx, client, cfg, plan)
				if singleErr != nil {
					results = append(results, updateResult{
						updatePlan: plan,
						Result:     "error",
						Error:      singleErr.Error(),
					})
					continue
				}
				results = append(results, verifyPlan(plan, result))
			}
			_ = err
			continue
		}

		verified, err := fetchProductsBatch(ctx, client, cfg, plansWooIDs(batch))
		if err != nil {
			for _, plan := range batch {
				results = append(results, updateResult{
					updatePlan: plan,
					Result:     "error",
					Error:      fmt.Sprintf("batch verify failed: %v", err),
				})
			}
			continue
		}

		byID := make(map[uint]wcProduct, len(verified))
		for _, product := range verified {
			byID[uint(product.ID)] = product
		}

		for _, plan := range batch {
			product, ok := byID[plan.WooID]
			if !ok {
				results = append(results, updateResult{
					updatePlan: plan,
					Result:     "error",
					Error:      "product missing in verify response",
				})
				continue
			}
			results = append(results, verifyPlan(plan, product))
		}

		_ = products
	}

	return results
}

func updateBatch(ctx context.Context, client *http.Client, cfg runtimeConfig, batch []updatePlan) ([]wcProduct, error) {
	base, err := url.Parse(cfg.Integrations.WooCommerce.BaseURL)
	if err != nil {
		return nil, err
	}
	base.Path = "/wp-json/wc/v3/products/batch"

	updates := make([]map[string]any, 0, len(batch))
	for _, plan := range batch {
		updates = append(updates, buildUpdatePayload(plan))
	}

	rawBody, err := json.Marshal(map[string]any{"update": updates})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base.String(), bytes.NewReader(rawBody))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(cfg.Integrations.WooCommerce.ConsumerKey, cfg.Integrations.WooCommerce.ConsumerSecret)
	req.Header.Set("User-Agent", "PCM2WWW/1.0")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var payload map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&payload); err == nil {
			if raw, merr := json.Marshal(payload); merr == nil {
				return nil, fmt.Errorf("batch POST http %d: %s", resp.StatusCode, string(raw))
			}
		}
		return nil, fmt.Errorf("batch POST http %d", resp.StatusCode)
	}

	var batchResp batchResponse
	if err := json.NewDecoder(resp.Body).Decode(&batchResp); err != nil {
		return nil, err
	}
	return batchResp.Update, nil
}

func updateSingle(ctx context.Context, client *http.Client, cfg runtimeConfig, plan updatePlan) (wcProduct, error) {
	base, err := url.Parse(cfg.Integrations.WooCommerce.BaseURL)
	if err != nil {
		return wcProduct{}, err
	}
	base.Path = fmt.Sprintf("/wp-json/wc/v3/products/%d", plan.WooID)

	rawBody, err := json.Marshal(buildUpdatePayload(plan))
	if err != nil {
		return wcProduct{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, base.String(), bytes.NewReader(rawBody))
	if err != nil {
		return wcProduct{}, err
	}
	req.SetBasicAuth(cfg.Integrations.WooCommerce.ConsumerKey, cfg.Integrations.WooCommerce.ConsumerSecret)
	req.Header.Set("User-Agent", "PCM2WWW/1.0")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return wcProduct{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var payload map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&payload); err == nil {
			if raw, merr := json.Marshal(payload); merr == nil {
				return wcProduct{}, fmt.Errorf("single PUT http %d: %s", resp.StatusCode, string(raw))
			}
		}
		return wcProduct{}, fmt.Errorf("single PUT http %d", resp.StatusCode)
	}

	var product wcProduct
	if err := json.NewDecoder(resp.Body).Decode(&product); err != nil {
		return wcProduct{}, err
	}
	return product, nil
}

func fetchProductsBatch(ctx context.Context, client *http.Client, cfg runtimeConfig, wooIDs []uint) ([]wcProduct, error) {
	if len(wooIDs) == 0 {
		return nil, nil
	}
	base, err := url.Parse(cfg.Integrations.WooCommerce.BaseURL)
	if err != nil {
		return nil, err
	}
	base.Path = "/wp-json/wc/v3/products"
	idStrings := make([]string, len(wooIDs))
	for i, id := range wooIDs {
		idStrings[i] = strconv.FormatUint(uint64(id), 10)
	}
	q := base.Query()
	q.Set("include", strings.Join(idStrings, ","))
	q.Set("per_page", strconv.Itoa(len(wooIDs)))
	q.Set("_fields", "id,sku,name,regular_price,sale_price,hurt_price,tax_class,manage_stock,stock_quantity,stock_status,meta_data")
	base.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(cfg.Integrations.WooCommerce.ConsumerKey, cfg.Integrations.WooCommerce.ConsumerSecret)
	req.Header.Set("User-Agent", "PCM2WWW/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("verify GET http %d", resp.StatusCode)
	}

	var products []wcProduct
	if err := json.NewDecoder(resp.Body).Decode(&products); err != nil {
		return nil, err
	}
	return products, nil
}

func buildUpdatePayload(plan updatePlan) map[string]any {
	stockStatus := "outofstock"
	if plan.DesiredStock > 0 {
		stockStatus = "instock"
	}

	return map[string]any{
		"id":             plan.WooID,
		"regular_price":  xlstowary.FormatMoney(plan.DesiredRegularGross),
		"sale_price":     "",
		"tax_class":      plan.DesiredTaxClass,
		"manage_stock":   true,
		"stock_quantity": plan.DesiredStock,
		"stock_status":   stockStatus,
		"hurt_price":     xlstowary.FormatMoney(plan.DesiredHurtGross),
		"meta_data": []map[string]any{
			{"key": "_hurt_price", "value": xlstowary.FormatMoney(plan.DesiredHurtGross)},
		},
	}
}

func verifyPlan(plan updatePlan, product wcProduct) updateResult {
	result := updateResult{
		updatePlan:           plan,
		Result:               "done",
		VerifiedRegularGross: parsePrice(product.RegularPrice),
		VerifiedHurtGross:    parsePrice(productHurtPrice(product)),
		VerifiedTaxClass:     strings.TrimSpace(product.TaxClass),
		VerifiedStock:        product.StockQuantity,
		VerifiedSalePrice:    parsePrice(product.SalePrice),
		VerifiedStockStatus:  strings.TrimSpace(product.StockStatus),
	}

	var problems []string
	if !xlstowary.SameMoney(result.VerifiedRegularGross, plan.DesiredRegularGross) {
		problems = append(problems, "regular_price")
	}
	if !xlstowary.SameMoney(result.VerifiedHurtGross, plan.DesiredHurtGross) {
		problems = append(problems, "hurt_price")
	}
	if strings.TrimSpace(result.VerifiedTaxClass) != strings.TrimSpace(plan.DesiredTaxClass) {
		problems = append(problems, "tax_class")
	}
	if int(xlstowary.RoundMoney(result.VerifiedStock)) != plan.DesiredStock {
		problems = append(problems, "stock")
	}
	if result.VerifiedSalePrice != 0 {
		problems = append(problems, "sale_price")
	}
	if len(problems) > 0 {
		result.Result = "verify_error"
		result.Error = "verification mismatch: " + strings.Join(problems, ", ")
	}
	return result
}

func productHurtPrice(product wcProduct) string {
	if strings.TrimSpace(product.HurtPrice) != "" {
		return product.HurtPrice
	}
	for _, meta := range product.MetaData {
		if meta.Key == "_hurt_price" {
			return strings.TrimSpace(fmt.Sprint(meta.Value))
		}
	}
	return ""
}

func plansWooIDs(plans []updatePlan) []uint {
	ids := make([]uint, 0, len(plans))
	for _, plan := range plans {
		ids = append(ids, plan.WooID)
	}
	return ids
}

func parsePrice(raw string) float64 {
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

func writePlans(path string, plans []updatePlan) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	w := csv.NewWriter(file)
	defer w.Flush()

	header := []string{
		"woo_id",
		"shop_sku",
		"shop_ean",
		"shop_name",
		"xls_towar_id",
		"xls_name",
		"desired_regular_gross",
		"desired_hurt_gross",
		"desired_tax_class",
		"desired_stock",
		"desired_stock_raw",
	}
	if err := w.Write(header); err != nil {
		return err
	}
	for _, plan := range plans {
		record := []string{
			strconv.FormatUint(uint64(plan.WooID), 10),
			plan.ShopSKU,
			plan.ShopEAN,
			plan.ShopName,
			strconv.FormatInt(plan.XLSTowarID, 10),
			plan.XLSName,
			xlstowary.FormatMoney(plan.DesiredRegularGross),
			xlstowary.FormatMoney(plan.DesiredHurtGross),
			plan.DesiredTaxClass,
			strconv.Itoa(plan.DesiredStock),
			xlstowary.FormatMoney(plan.DesiredStockRaw),
		}
		if err := w.Write(record); err != nil {
			return err
		}
	}
	return w.Error()
}

func writeResults(path string, results []updateResult) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	w := csv.NewWriter(file)
	defer w.Flush()

	header := []string{
		"woo_id",
		"shop_sku",
		"shop_ean",
		"shop_name",
		"xls_towar_id",
		"xls_name",
		"desired_regular_gross",
		"desired_hurt_gross",
		"desired_tax_class",
		"desired_stock",
		"result",
		"error",
		"verified_regular_gross",
		"verified_hurt_gross",
		"verified_tax_class",
		"verified_stock",
		"verified_sale_price",
		"verified_stock_status",
	}
	if err := w.Write(header); err != nil {
		return err
	}
	for _, result := range results {
		record := []string{
			strconv.FormatUint(uint64(result.WooID), 10),
			result.ShopSKU,
			result.ShopEAN,
			result.ShopName,
			strconv.FormatInt(result.XLSTowarID, 10),
			result.XLSName,
			xlstowary.FormatMoney(result.DesiredRegularGross),
			xlstowary.FormatMoney(result.DesiredHurtGross),
			result.DesiredTaxClass,
			strconv.Itoa(result.DesiredStock),
			result.Result,
			result.Error,
			xlstowary.FormatMoney(result.VerifiedRegularGross),
			xlstowary.FormatMoney(result.VerifiedHurtGross),
			result.VerifiedTaxClass,
			xlstowary.FormatMoney(result.VerifiedStock),
			xlstowary.FormatMoney(result.VerifiedSalePrice),
			result.VerifiedStockStatus,
		}
		if err := w.Write(record); err != nil {
			return err
		}
	}
	return w.Error()
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
