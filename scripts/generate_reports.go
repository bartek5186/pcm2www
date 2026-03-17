package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

var reDigits = regexp.MustCompile(`\D+`)

var nameReplacer = strings.NewReplacer(
	"ą", "a", "ć", "c", "ę", "e", "ł", "l", "ń", "n", "ó", "o", "ś", "s", "ź", "z", "ż", "z",
	"Ą", "a", "Ć", "c", "Ę", "e", "Ł", "l", "Ń", "n", "Ó", "o", "Ś", "s", "Ź", "z", "Ż", "z",
	"ă", "a", "â", "a", "î", "i", "ș", "s", "ş", "s", "ț", "t", "ţ", "t",
	"Ă", "a", "Â", "a", "Î", "i", "Ș", "s", "Ş", "s", "Ț", "t", "Ţ", "t",
	"á", "a", "à", "a", "ä", "a", "â", "a", "ã", "a", "å", "a",
	"é", "e", "è", "e", "ë", "e", "ê", "e",
	"í", "i", "ì", "i", "ï", "i", "î", "i",
	"ó", "o", "ò", "o", "ö", "o", "ô", "o", "õ", "o",
	"ú", "u", "ù", "u", "ü", "u", "û", "u",
	"ý", "y", "ÿ", "y",
)

var nameStopWords = map[string]struct{}{
	"aoc": {}, "bio": {}, "brut": {}, "demi": {}, "doc": {}, "docg": {}, "dop": {},
	"do": {}, "dry": {}, "edition": {}, "extra": {}, "ig": {}, "igt": {}, "igp": {},
	"millesimato": {}, "organic": {}, "reserve": {}, "reserva": {}, "semi": {}, "special": {},
	"vegan": {}, "vintage": {},
}

type ShopProduct struct {
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

type MagazineProduct struct {
	TowarID       int64   `gorm:"column:towar_id"`
	MagEAN        string  `gorm:"column:mag_ean"`
	MagName       string  `gorm:"column:mag_name"`
	CenaDetal     float64 `gorm:"column:cena_detal"`
	CenaHurtowa   float64 `gorm:"column:cena_hurtowa"`
	AktywnyWSI    int     `gorm:"column:aktywny_wsi"`
	DoUsuniecia   int     `gorm:"column:do_usuniecia"`
	TotalStock    float64 `gorm:"column:total_stock"`
	TotalReserved float64 `gorm:"column:total_reserved"`
	ImportID      uint    `gorm:"column:import_id"`
	UpdatedAt     string  `gorm:"column:updated_at"`
}

type DifferenceRow struct {
	DifferenceType string
	LocalTowarID   string
	LocalEAN       string
	LocalName      string
	ShopWooID      string
	ShopSKU        string
	ShopEAN        string
	ShopName       string
	Note           string
}

type ShopMissingEANNameCandidate struct {
	ShopWooID        uint
	ShopSKU          string
	ShopName         string
	CandidateRank    int
	MatchQuality     string
	MatchScore       float64
	SharedTokens     string
	SharedTokenCount int
	ShopTokenCount   int
	MagTokenCount    int
	MagTowarID       int64
	MagEAN           string
	MagName          string
	MagStock         float64
	MagPrice         float64
}

func main() {
	home, _ := os.UserHomeDir()
	defaultDB := filepath.Join(home, ".config", "pcm2www", "pcm2www.db")

	dbPath := flag.String("db", defaultDB, "path to pcm2www sqlite database")
	outDir := flag.String("out", "reports", "directory for generated csv reports")
	flag.Parse()

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fatalf("mkdir %s: %v", *outDir, err)
	}

	gdb, err := gorm.Open(sqlite.Open(*dbPath), &gorm.Config{})
	if err != nil {
		fatalf("open db %s: %v", *dbPath, err)
	}

	shopRows, err := loadShopProducts(gdb)
	if err != nil {
		fatalf("load shop products: %v", err)
	}
	magRows, err := loadMagazineProducts(gdb)
	if err != nil {
		fatalf("load magazine products: %v", err)
	}
	diffRows := buildDifferences(shopRows, magRows)
	nameCandidates := buildShopMissingEANNameCandidates(shopRows, magRows)

	sort.Slice(diffRows, func(i, j int) bool {
		if diffRows[i].DifferenceType != diffRows[j].DifferenceType {
			return diffRows[i].DifferenceType < diffRows[j].DifferenceType
		}
		if diffRows[i].LocalTowarID != diffRows[j].LocalTowarID {
			return diffRows[i].LocalTowarID < diffRows[j].LocalTowarID
		}
		return diffRows[i].ShopWooID < diffRows[j].ShopWooID
	})

	if err := writeShopProducts(filepath.Join(*outDir, "shop_products.csv"), shopRows); err != nil {
		fatalf("write shop report: %v", err)
	}
	if err := writeMagazineProducts(filepath.Join(*outDir, "magazine_products.csv"), magRows); err != nil {
		fatalf("write magazine report: %v", err)
	}
	if err := writeDifferences(filepath.Join(*outDir, "shop_magazine_differences.csv"), diffRows); err != nil {
		fatalf("write differences report: %v", err)
	}
	if err := writeShopMissingEANNameCandidates(filepath.Join(*outDir, "shop_missing_ean_name_candidates.csv"), nameCandidates); err != nil {
		fatalf("write shop missing ean name candidates report: %v", err)
	}

	counts := make(map[string]int)
	for _, row := range diffRows {
		counts[row.DifferenceType]++
	}

	fmt.Printf("Generated %d shop rows, %d magazine rows, %d difference rows at %s\n", len(shopRows), len(magRows), len(diffRows), time.Now().Format(time.RFC3339))
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%s=%d\n", k, counts[k])
	}
	fmt.Printf("SHOP_MISSING_EAN_NAME_CANDIDATES=%d\n", len(nameCandidates))
}

func loadShopProducts(gdb *gorm.DB) ([]ShopProduct, error) {
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
	var rows []ShopProduct
	return rows, gdb.Raw(q).Scan(&rows).Error
}

func loadMagazineProducts(gdb *gorm.DB) ([]MagazineProduct, error) {
	const q = `
SELECT
	p.towar_id,
	p.kod AS mag_ean,
	p.nazwa AS mag_name,
	p.cena_detal,
	p.cena_hurtowa,
	p.aktywny_wsi,
	p.do_usuniecia,
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
	p.aktywny_wsi,
	p.do_usuniecia,
	p.import_id,
	p.updated_at
ORDER BY p.towar_id;
`
	var rows []MagazineProduct
	return rows, gdb.Raw(q).Scan(&rows).Error
}

func buildDifferences(shopRows []ShopProduct, magRows []MagazineProduct) []DifferenceRow {
	shopByEAN := make(map[string][]ShopProduct)
	magByEAN := make(map[string][]MagazineProduct)

	for _, row := range shopRows {
		if key := cleanEAN(row.ShopEAN); key != "" {
			shopByEAN[key] = append(shopByEAN[key], row)
		}
	}
	for _, row := range magRows {
		if key := cleanEAN(row.MagEAN); key != "" {
			magByEAN[key] = append(magByEAN[key], row)
		}
	}

	out := make([]DifferenceRow, 0, len(shopRows)+len(magRows))

	for _, row := range magRows {
		ean := cleanEAN(row.MagEAN)
		switch {
		case ean == "":
			out = append(out, DifferenceRow{
				DifferenceType: "MAGAZINE_MISSING_EAN",
				LocalTowarID:   strconv.FormatInt(row.TowarID, 10),
				LocalEAN:       row.MagEAN,
				LocalName:      row.MagName,
				Note:           "Pole kod/EAN w magazynie jest puste lub nienumeryczne",
			})
		case len(magByEAN[ean]) > 1:
			out = append(out, DifferenceRow{
				DifferenceType: "DUPLICATE_MAGAZINE_EAN",
				LocalTowarID:   strconv.FormatInt(row.TowarID, 10),
				LocalEAN:       row.MagEAN,
				LocalName:      row.MagName,
				Note:           "Ten EAN występuje wielokrotnie po stronie magazynu",
			})
		case len(shopByEAN[ean]) == 0:
			out = append(out, DifferenceRow{
				DifferenceType: "MAGAZINE_NOT_IN_SHOP_BY_EAN",
				LocalTowarID:   strconv.FormatInt(row.TowarID, 10),
				LocalEAN:       row.MagEAN,
				LocalName:      row.MagName,
				Note:           "Brak produktu w sklepie z takim EAN",
			})
		case len(shopByEAN[ean]) > 1:
			out = append(out, DifferenceRow{
				DifferenceType: "DUPLICATE_SHOP_EAN",
				LocalTowarID:   strconv.FormatInt(row.TowarID, 10),
				LocalEAN:       row.MagEAN,
				LocalName:      row.MagName,
				Note:           "Ten EAN występuje wielokrotnie po stronie sklepu",
			})
		}
	}

	for _, row := range shopRows {
		ean := cleanEAN(row.ShopEAN)
		switch {
		case ean == "":
			out = append(out, DifferenceRow{
				DifferenceType: "SHOP_MISSING_EAN",
				ShopWooID:      strconv.FormatUint(uint64(row.WooID), 10),
				ShopSKU:        row.ShopSKU,
				ShopEAN:        row.ShopEAN,
				ShopName:       row.ShopName,
				Note:           "Produkt sklepowy nie ma EAN, więc nie można go porównać po EAN",
			})
		case len(shopByEAN[ean]) > 1:
			out = append(out, DifferenceRow{
				DifferenceType: "DUPLICATE_SHOP_EAN",
				ShopWooID:      strconv.FormatUint(uint64(row.WooID), 10),
				ShopSKU:        row.ShopSKU,
				ShopEAN:        row.ShopEAN,
				ShopName:       row.ShopName,
				Note:           "Ten EAN występuje wielokrotnie po stronie sklepu",
			})
		case len(magByEAN[ean]) == 0:
			out = append(out, DifferenceRow{
				DifferenceType: "SHOP_NOT_IN_MAGAZINE_BY_EAN",
				ShopWooID:      strconv.FormatUint(uint64(row.WooID), 10),
				ShopSKU:        row.ShopSKU,
				ShopEAN:        row.ShopEAN,
				ShopName:       row.ShopName,
				Note:           "Brak produktu w magazynie z takim EAN",
			})
		case len(magByEAN[ean]) > 1:
			out = append(out, DifferenceRow{
				DifferenceType: "DUPLICATE_MAGAZINE_EAN",
				ShopWooID:      strconv.FormatUint(uint64(row.WooID), 10),
				ShopSKU:        row.ShopSKU,
				ShopEAN:        row.ShopEAN,
				ShopName:       row.ShopName,
				Note:           "Ten EAN występuje wielokrotnie po stronie magazynu",
			})
		}
	}

	return out
}

func buildShopMissingEANNameCandidates(shopRows []ShopProduct, magRows []MagazineProduct) []ShopMissingEANNameCandidate {
	type preparedMagazine struct {
		row      MagazineProduct
		normName string
		tokens   []string
		tokenSet map[string]struct{}
	}

	type scoredCandidate struct {
		row       MagazineProduct
		score     float64
		quality   string
		shared    []string
		shopCount int
		magCount  int
	}

	preparedMag := make([]preparedMagazine, 0, len(magRows))
	for _, row := range magRows {
		if cleanEAN(row.MagEAN) == "" {
			continue
		}
		normName, tokens, tokenSet := prepareNameForMatch(row.MagName)
		if normName == "" || len(tokens) == 0 {
			continue
		}
		preparedMag = append(preparedMag, preparedMagazine{
			row:      row,
			normName: normName,
			tokens:   tokens,
			tokenSet: tokenSet,
		})
	}

	out := make([]ShopMissingEANNameCandidate, 0)
	for _, shop := range shopRows {
		if cleanEAN(shop.ShopEAN) != "" {
			continue
		}

		shopNorm, shopTokens, shopSet := prepareNameForMatch(shop.ShopName)
		if shopNorm == "" || len(shopTokens) == 0 {
			continue
		}

		candidates := make([]scoredCandidate, 0, 8)
		for _, mag := range preparedMag {
			score, quality, shared, ok := scoreNameMatch(shopNorm, shopTokens, shopSet, mag.normName, mag.tokens, mag.tokenSet)
			if !ok {
				continue
			}
			candidates = append(candidates, scoredCandidate{
				row:       mag.row,
				score:     score,
				quality:   quality,
				shared:    shared,
				shopCount: len(shopTokens),
				magCount:  len(mag.tokens),
			})
		}

		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].score != candidates[j].score {
				return candidates[i].score > candidates[j].score
			}
			if len(candidates[i].shared) != len(candidates[j].shared) {
				return len(candidates[i].shared) > len(candidates[j].shared)
			}
			if candidates[i].row.TotalStock != candidates[j].row.TotalStock {
				return candidates[i].row.TotalStock > candidates[j].row.TotalStock
			}
			return candidates[i].row.TowarID < candidates[j].row.TowarID
		})

		if len(candidates) > 5 {
			candidates = candidates[:5]
		}

		for idx, candidate := range candidates {
			out = append(out, ShopMissingEANNameCandidate{
				ShopWooID:        shop.WooID,
				ShopSKU:          shop.ShopSKU,
				ShopName:         shop.ShopName,
				CandidateRank:    idx + 1,
				MatchQuality:     candidate.quality,
				MatchScore:       candidate.score,
				SharedTokens:     strings.Join(candidate.shared, "|"),
				SharedTokenCount: len(candidate.shared),
				ShopTokenCount:   candidate.shopCount,
				MagTokenCount:    candidate.magCount,
				MagTowarID:       candidate.row.TowarID,
				MagEAN:           candidate.row.MagEAN,
				MagName:          candidate.row.MagName,
				MagStock:         candidate.row.TotalStock,
				MagPrice:         candidate.row.CenaDetal,
			})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].ShopWooID != out[j].ShopWooID {
			return out[i].ShopWooID < out[j].ShopWooID
		}
		return out[i].CandidateRank < out[j].CandidateRank
	})

	return out
}

func writeShopProducts(path string, rows []ShopProduct) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	w := csv.NewWriter(file)
	defer w.Flush()

	if err := w.Write([]string{
		"woo_id", "shop_sku", "shop_ean", "shop_name", "shop_status", "shop_type",
		"stock_qty", "price_regular", "price_sale", "hurt_price", "date_modified",
	}); err != nil {
		return err
	}
	for _, row := range rows {
		if err := w.Write([]string{
			strconv.FormatUint(uint64(row.WooID), 10),
			row.ShopSKU,
			row.ShopEAN,
			row.ShopName,
			row.ShopStatus,
			row.ShopType,
			formatFloat(row.StockQty),
			formatFloat(row.PriceRegular),
			formatFloat(row.PriceSale),
			formatFloat(row.HurtPrice),
			row.DateModified,
		}); err != nil {
			return err
		}
	}
	return w.Error()
}

func writeMagazineProducts(path string, rows []MagazineProduct) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	w := csv.NewWriter(file)
	defer w.Flush()

	if err := w.Write([]string{
		"towar_id", "mag_ean", "mag_name", "cena_detal", "cena_hurtowa", "aktywny_wsi",
		"do_usuniecia", "total_stock", "total_reserved", "import_id", "updated_at",
	}); err != nil {
		return err
	}
	for _, row := range rows {
		if err := w.Write([]string{
			strconv.FormatInt(row.TowarID, 10),
			row.MagEAN,
			row.MagName,
			formatFloat(row.CenaDetal),
			formatFloat(row.CenaHurtowa),
			strconv.Itoa(row.AktywnyWSI),
			strconv.Itoa(row.DoUsuniecia),
			formatFloat(row.TotalStock),
			formatFloat(row.TotalReserved),
			strconv.FormatUint(uint64(row.ImportID), 10),
			row.UpdatedAt,
		}); err != nil {
			return err
		}
	}
	return w.Error()
}

func writeDifferences(path string, rows []DifferenceRow) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	w := csv.NewWriter(file)
	defer w.Flush()

	if err := w.Write([]string{
		"difference_type", "local_towar_id", "local_ean", "local_name",
		"shop_woo_id", "shop_sku", "shop_ean", "shop_name", "note",
	}); err != nil {
		return err
	}
	for _, row := range rows {
		if err := w.Write([]string{
			row.DifferenceType,
			row.LocalTowarID,
			row.LocalEAN,
			row.LocalName,
			row.ShopWooID,
			row.ShopSKU,
			row.ShopEAN,
			row.ShopName,
			row.Note,
		}); err != nil {
			return err
		}
	}
	return w.Error()
}

func writeShopMissingEANNameCandidates(path string, rows []ShopMissingEANNameCandidate) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	w := csv.NewWriter(file)
	defer w.Flush()

	if err := w.Write([]string{
		"shop_woo_id", "shop_sku", "shop_name", "candidate_rank", "match_quality", "match_score",
		"shared_tokens", "shared_token_count", "shop_token_count", "mag_token_count",
		"mag_towar_id", "mag_ean", "mag_name", "mag_stock", "mag_price",
	}); err != nil {
		return err
	}

	for _, row := range rows {
		if err := w.Write([]string{
			strconv.FormatUint(uint64(row.ShopWooID), 10),
			row.ShopSKU,
			row.ShopName,
			strconv.Itoa(row.CandidateRank),
			row.MatchQuality,
			formatFloat(row.MatchScore),
			row.SharedTokens,
			strconv.Itoa(row.SharedTokenCount),
			strconv.Itoa(row.ShopTokenCount),
			strconv.Itoa(row.MagTokenCount),
			strconv.FormatInt(row.MagTowarID, 10),
			row.MagEAN,
			row.MagName,
			formatFloat(row.MagStock),
			formatFloat(row.MagPrice),
		}); err != nil {
			return err
		}
	}

	return w.Error()
}

func cleanEAN(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return reDigits.ReplaceAllString(s, "")
}

func normalizeName(s string) string {
	s = strings.TrimSpace(strings.ToLower(nameReplacer.Replace(s)))
	if s == "" {
		return ""
	}

	var b strings.Builder
	b.Grow(len(s))
	lastSpace := true
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastSpace = false
		case !lastSpace:
			b.WriteByte(' ')
			lastSpace = true
		}
	}

	return strings.TrimSpace(b.String())
}

func prepareNameForMatch(s string) (string, []string, map[string]struct{}) {
	norm := normalizeName(s)
	if norm == "" {
		return "", nil, nil
	}

	rawTokens := strings.Fields(norm)
	tokens := make([]string, 0, len(rawTokens))
	tokenSet := make(map[string]struct{}, len(rawTokens))
	for _, token := range rawTokens {
		if isNoiseToken(token) {
			continue
		}
		if _, ok := tokenSet[token]; ok {
			continue
		}
		tokenSet[token] = struct{}{}
		tokens = append(tokens, token)
	}

	if len(tokens) == 0 {
		return norm, nil, nil
	}

	return norm, tokens, tokenSet
}

func isNoiseToken(token string) bool {
	if token == "" {
		return true
	}
	if _, ok := nameStopWords[token]; ok {
		return true
	}
	if len(token) == 4 && (strings.HasPrefix(token, "19") || strings.HasPrefix(token, "20")) {
		return true
	}
	return false
}

func scoreNameMatch(shopNorm string, shopTokens []string, shopSet map[string]struct{}, magNorm string, magTokens []string, magSet map[string]struct{}) (float64, string, []string, bool) {
	if len(shopTokens) == 0 || len(magTokens) == 0 {
		return 0, "", nil, false
	}

	shared := make([]string, 0, minInt(len(shopTokens), len(magTokens)))
	for _, token := range shopTokens {
		if _, ok := magSet[token]; ok {
			shared = append(shared, token)
		}
	}

	if len(shared) == 0 {
		return 0, "", nil, false
	}

	union := len(shopSet)
	for token := range magSet {
		if _, ok := shopSet[token]; !ok {
			union++
		}
	}

	sharedCount := float64(len(shared))
	containment := sharedCount / float64(minInt(len(shopSet), len(magSet)))
	jaccard := sharedCount / float64(union)
	dice := diceCoefficient(shopNorm, magNorm)
	score := 0.50*containment + 0.30*jaccard + 0.20*dice

	if shopNorm == magNorm {
		return 1, "exact_name", shared, true
	}
	if strings.Contains(shopNorm, magNorm) || strings.Contains(magNorm, shopNorm) {
		score += 0.05
	}
	if score > 0.99 {
		score = 0.99
	}

	switch {
	case len(shared) >= 3 && score >= 0.70:
		return score, "strong_name_match", shared, true
	case len(shared) >= 2 && score >= 0.55:
		return score, "medium_name_match", shared, true
	case len(shared) >= 1 && dice >= 0.82:
		return score, "fuzzy_name_match", shared, true
	default:
		return 0, "", nil, false
	}
}

func diceCoefficient(a, b string) float64 {
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1
	}

	aBigrams := make(map[string]int)
	for _, gram := range stringBigrams(a) {
		aBigrams[gram]++
	}

	common := 0
	for _, gram := range stringBigrams(b) {
		if aBigrams[gram] <= 0 {
			continue
		}
		common++
		aBigrams[gram]--
	}

	total := len(stringBigrams(a)) + len(stringBigrams(b))
	if total == 0 {
		return 0
	}
	return float64(2*common) / float64(total)
}

func stringBigrams(s string) []string {
	runes := []rune(s)
	if len(runes) < 2 {
		return []string{s}
	}

	out := make([]string, 0, len(runes)-1)
	for i := 0; i < len(runes)-1; i++ {
		out = append(out, string(runes[i:i+2]))
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
