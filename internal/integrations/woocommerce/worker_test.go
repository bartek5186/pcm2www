package woocommerce

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/bartek5186/pcm2www/internal/db"
	"github.com/rs/zerolog"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func TestWorkerTickAppliesAndVerifiesTasks(t *testing.T) {
	state := map[uint]wcProduct{
		10: {
			ID:             10,
			Name:           "Worker Product",
			SKU:            "SKU-10",
			GlobalUniqueID: "",
			RegularPrice:   "20",
			SalePrice:      "0",
			HurtPrice:      "10",
			MetaData:       []wcMetaData{{Key: "_hurt_price", Value: "10"}},
			ManageStock:    true,
			StockQuantity:  1,
			Status:         "publish",
			Type:           "simple",
		},
	}

	client := newWooWorkerTestClient(t, state)

	gdb := newWooWorkerTestDB(t)
	towarID := int64(101)
	wooID := uint(10)

	if err := gdb.Create(&db.ImportFile{ImportID: 1, Filename: "exp_wyk_worker.xml", Status: 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&db.WooProductCache{
		WooID:        wooID,
		TowarID:      &towarID,
		Kod:          "SKU-10",
		Name:         "Worker Product",
		PriceRegular: 20,
		HurtPrice:    10,
		StockQty:     1,
		StockManaged: true,
		Status:       "publish",
		Type:         "simple",
	}).Error; err != nil {
		t.Fatal(err)
	}

	eanPayload, _ := json.Marshal(db.WooEANUpdatePayload{ImportID: 1, WooID: wooID, TowarID: towarID, DesiredEAN: "5901234567890"})
	stockPayload, _ := json.Marshal(db.WooStockUpdatePayload{ImportID: 1, WooID: wooID, TowarID: towarID, DesiredStock: 4})
	pricePayload, _ := json.Marshal(db.WooPriceUpdatePayload{ImportID: 1, WooID: wooID, TowarID: towarID, DesiredRegular: 25, DesiredHurt: 17})

	if err := gdb.Create([]db.WooTask{
		{TaskKey: "ean.update:10:5901234567890", ImportID: 1, TowarID: &towarID, WooID: &wooID, Kind: db.WooTaskKindEANUpdate, PayloadJSON: string(eanPayload), Status: "pending"},
		{TaskKey: "stock.update:10:4", ImportID: 1, TowarID: &towarID, WooID: &wooID, Kind: db.WooTaskKindStockUpdate, PayloadJSON: string(stockPayload), Status: "pending"},
		{TaskKey: "price.update:10:25:17", ImportID: 1, TowarID: &towarID, WooID: &wooID, Kind: db.WooTaskKindPriceUpdate, PayloadJSON: string(pricePayload), Status: "pending"},
	}).Error; err != nil {
		t.Fatal(err)
	}

	w := &Woo{
		log:  zerolog.Nop(),
		cfg:  Config{BaseURL: "https://woo.test", ConsumerKey: "ck", ConsumerSec: "cs"},
		http: client,
	}

	w.workerTick(context.Background(), gdb)

	var tasks []db.WooTask
	if err := gdb.Order("kind asc").Find(&tasks).Error; err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if task.Status != "done" {
			t.Fatalf("expected done task, got %+v", task)
		}
	}

	verified := state[wooID]
	if verified.GlobalUniqueID != "5901234567890" {
		t.Fatalf("expected updated ean, got %q", verified.GlobalUniqueID)
	}
	if verified.StockQuantity != 4 {
		t.Fatalf("expected updated stock, got %v", verified.StockQuantity)
	}
	if verified.RegularPrice != "25" || verified.HurtPrice != "17" {
		t.Fatalf("expected updated prices, got regular=%s hurt=%s", verified.RegularPrice, verified.HurtPrice)
	}

	var cache db.WooProductCache
	if err := gdb.Where("woo_id = ?", wooID).Take(&cache).Error; err != nil {
		t.Fatal(err)
	}
	if cache.Ean != "5901234567890" || cache.StockQty != 4 || cache.PriceRegular != 25 || cache.HurtPrice != 17 {
		t.Fatalf("cache not updated after worker: %+v", cache)
	}
}

func TestWorkerTickAppliesSafetyPolicy(t *testing.T) {
	state := map[uint]wcProduct{
		10: {
			ID:             10,
			Name:           "Has EAN",
			SKU:            "SKU-10",
			GlobalUniqueID: "1111111111111",
			RegularPrice:   "20",
			SalePrice:      "0",
			HurtPrice:      "10",
			MetaData:       []wcMetaData{{Key: "_hurt_price", Value: "10"}},
			ManageStock:    true,
			StockQuantity:  1,
			Status:         "publish",
			Type:           "simple",
		},
		11: {
			ID:            11,
			Name:          "No stock manage",
			SKU:           "SKU-11",
			RegularPrice:  "20",
			SalePrice:     "0",
			HurtPrice:     "10",
			ManageStock:   false,
			StockQuantity: 1,
			Status:        "publish",
			Type:          "simple",
		},
		12: {
			ID:            12,
			Name:          "Sale active",
			SKU:           "SKU-12",
			RegularPrice:  "20",
			SalePrice:     "5",
			HurtPrice:     "10",
			ManageStock:   true,
			StockQuantity: 1,
			Status:        "publish",
			Type:          "simple",
		},
	}

	client := newWooWorkerTestClient(t, state)

	gdb := newWooWorkerTestDB(t)
	importID := uint(2)
	if err := gdb.Create(&db.ImportFile{ImportID: importID, Filename: "exp_wyk_policy.xml", Status: 1}).Error; err != nil {
		t.Fatal(err)
	}

	towarA, towarB, towarC := int64(201), int64(202), int64(203)
	wooA, wooB, wooC := uint(10), uint(11), uint(12)
	if err := gdb.Create([]db.WooProductCache{
		{WooID: wooA, TowarID: &towarA, Kod: "SKU-10", Ean: "1111111111111", Name: "Has EAN", StockManaged: true, StockQty: 1, PriceRegular: 20, HurtPrice: 10},
		{WooID: wooB, TowarID: &towarB, Kod: "SKU-11", Ean: "", Name: "No stock manage", StockManaged: false, StockQty: 1, PriceRegular: 20, HurtPrice: 10},
		{WooID: wooC, TowarID: &towarC, Kod: "SKU-12", Ean: "", Name: "Sale active", StockManaged: true, StockQty: 1, PriceRegular: 20, PriceSale: 5, HurtPrice: 10},
		{WooID: 99, Kod: "OWNER", Ean: "5909999999999", Name: "Duplicate owner"},
	}).Error; err != nil {
		t.Fatal(err)
	}

	eanPayload, _ := json.Marshal(db.WooEANUpdatePayload{ImportID: importID, WooID: wooA, TowarID: towarA, DesiredEAN: "5909999999999"})
	stockPayload, _ := json.Marshal(db.WooStockUpdatePayload{ImportID: importID, WooID: wooB, TowarID: towarB, DesiredStock: 4})
	pricePayload, _ := json.Marshal(db.WooPriceUpdatePayload{ImportID: importID, WooID: wooC, TowarID: towarC, DesiredRegular: 25, DesiredHurt: 17})

	if err := gdb.Create([]db.WooTask{
		{TaskKey: "ean.update:10:5909999999999", ImportID: importID, TowarID: &towarA, WooID: &wooA, Kind: db.WooTaskKindEANUpdate, PayloadJSON: string(eanPayload), Status: "pending"},
		{TaskKey: "stock.update:11:4", ImportID: importID, TowarID: &towarB, WooID: &wooB, Kind: db.WooTaskKindStockUpdate, PayloadJSON: string(stockPayload), Status: "pending"},
		{TaskKey: "price.update:12:25:17", ImportID: importID, TowarID: &towarC, WooID: &wooC, Kind: db.WooTaskKindPriceUpdate, PayloadJSON: string(pricePayload), Status: "pending"},
	}).Error; err != nil {
		t.Fatal(err)
	}

	w := &Woo{
		log:  zerolog.Nop(),
		cfg:  Config{BaseURL: "https://woo.test", ConsumerKey: "ck", ConsumerSec: "cs"},
		http: client,
	}

	w.workerTick(context.Background(), gdb)

	var rows []struct {
		Kind   string
		Status string
	}
	if err := gdb.Model(&db.WooTask{}).Select("kind", "status").Order("kind asc").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		db.WooTaskKindEANUpdate:   "skipped",
		db.WooTaskKindStockUpdate: "skipped",
		db.WooTaskKindPriceUpdate: "skipped",
	}
	for _, row := range rows {
		if want[row.Kind] != row.Status {
			t.Fatalf("unexpected status for %s: got %s want %s", row.Kind, row.Status, want[row.Kind])
		}
	}
}

func TestWorkerTickDoesNotClaimWhenContextCanceled(t *testing.T) {
	gdb := newWooWorkerTestDB(t)
	importID := uint(3)
	wooID := uint(10)
	towarID := int64(301)

	if err := gdb.Create(&db.ImportFile{ImportID: importID, Filename: "exp_wyk_canceled.xml", Status: 1}).Error; err != nil {
		t.Fatal(err)
	}

	payload, _ := json.Marshal(db.WooStockUpdatePayload{ImportID: importID, WooID: wooID, TowarID: towarID, DesiredStock: 4})
	if err := gdb.Create(&db.WooTask{
		TaskKey:     "stock.update:10:4",
		ImportID:    importID,
		TowarID:     &towarID,
		WooID:       &wooID,
		Kind:        db.WooTaskKindStockUpdate,
		PayloadJSON: string(payload),
		Status:      "pending",
	}).Error; err != nil {
		t.Fatal(err)
	}

	w := &Woo{log: zerolog.Nop()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	w.workerTick(ctx, gdb)

	var task db.WooTask
	if err := gdb.Where("task_key = ?", "stock.update:10:4").Take(&task).Error; err != nil {
		t.Fatal(err)
	}
	if task.Status != "pending" || task.Attempts != 0 {
		t.Fatalf("expected untouched pending task after canceled worker tick, got %+v", task)
	}
}

func TestExecuteWooTaskRequeuesInterruptedRequest(t *testing.T) {
	gdb := newWooWorkerTestDB(t)
	importID := uint(4)
	wooID := uint(10)
	towarID := int64(401)

	if err := gdb.Create(&db.ImportFile{ImportID: importID, Filename: "exp_wyk_interrupt.xml", Status: 1}).Error; err != nil {
		t.Fatal(err)
	}

	payload, _ := json.Marshal(db.WooStockUpdatePayload{ImportID: importID, WooID: wooID, TowarID: towarID, DesiredStock: 4})
	if err := gdb.Create(&db.WooTask{
		TaskKey:     "stock.update:10:4",
		ImportID:    importID,
		TowarID:     &towarID,
		WooID:       &wooID,
		Kind:        db.WooTaskKindStockUpdate,
		PayloadJSON: string(payload),
		Status:      "pending",
	}).Error; err != nil {
		t.Fatal(err)
	}

	task, err := claimNextWooTask(gdb)
	if err != nil {
		t.Fatal(err)
	}
	if task == nil {
		t.Fatal("expected claimed task")
	}

	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}

	w := &Woo{
		log:  zerolog.Nop(),
		cfg:  Config{BaseURL: "https://woo.test", ConsumerKey: "ck", ConsumerSec: "cs"},
		http: client,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.executeWooTask(ctx, gdb, *task)

	var refreshed db.WooTask
	if err := gdb.Where("task_id = ?", task.TaskID).Take(&refreshed).Error; err != nil {
		t.Fatal(err)
	}
	if refreshed.Status != "pending" {
		t.Fatalf("expected interrupted task to return to pending, got %+v", refreshed)
	}
	if refreshed.LastError != "" {
		t.Fatalf("expected interrupted task to clear last_error, got %+v", refreshed)
	}
	if refreshed.StartedAt != nil || refreshed.FinishedAt != nil {
		t.Fatalf("expected interrupted task timestamps to be reset, got %+v", refreshed)
	}
	if refreshed.Attempts != 1 {
		t.Fatalf("expected interrupted task to preserve attempt count, got %+v", refreshed)
	}
}

func newWooWorkerTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.AutoMigrate(
		&db.ImportFile{},
		&db.StProduct{},
		&db.StStock{},
		&db.WooProductCache{},
		&db.WooTask{},
		&db.KV{},
		&db.LinkIssue{},
	); err != nil {
		t.Fatal(err)
	}
	return gdb
}

func newWooWorkerTestClient(t *testing.T, state map[uint]wcProduct) *http.Client {
	t.Helper()

	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		path := strings.TrimPrefix(r.URL.Path, "/wp-json/wc/v3/products")
		path = strings.Trim(path, "/")
		if path == "" {
			var products []wcProduct
			for _, product := range state {
				products = append(products, product)
			}
			return jsonResponse(http.StatusOK, products)
		}

		id64, err := strconv.ParseUint(path, 10, 64)
		if err != nil {
			return textResponse(http.StatusBadRequest, "bad id"), nil
		}
		id := uint(id64)
		product, ok := state[id]
		if !ok {
			return textResponse(http.StatusNotFound, "not found"), nil
		}

		switch r.Method {
		case http.MethodGet:
			return jsonResponse(http.StatusOK, product)

		case http.MethodPut:
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				return textResponse(http.StatusBadRequest, "bad json"), nil
			}
			if raw, ok := body["global_unique_id"]; ok {
				product.GlobalUniqueID = fmt.Sprint(raw)
			}
			if raw, ok := body["stock_quantity"]; ok {
				switch v := raw.(type) {
				case float64:
					product.StockQuantity = v
				case string:
					f, _ := strconv.ParseFloat(v, 64)
					product.StockQuantity = f
				}
			}
			if raw, ok := body["regular_price"]; ok {
				product.RegularPrice = fmt.Sprint(raw)
			}
			if raw, ok := body["hurt_price"]; ok {
				_ = raw // top-level hurt_price is ignored by the live store
			}
			if raw, ok := body["meta_data"]; ok {
				applyMetaDataUpdate(&product, raw)
			}
			state[id] = product
			return jsonResponse(http.StatusOK, product)

		default:
			return textResponse(http.StatusMethodNotAllowed, "method not allowed"), nil
		}
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return fn(r)
}

func jsonResponse(status int, v any) (*http.Response, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(raw))),
	}, nil
}

func textResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func applyMetaDataUpdate(product *wcProduct, raw any) {
	items, ok := raw.([]any)
	if !ok {
		return
	}

	for _, item := range items {
		row, ok := item.(map[string]any)
		if !ok {
			continue
		}

		key := strings.TrimSpace(fmt.Sprint(row["key"]))
		if key == "" {
			continue
		}
		value := fmt.Sprint(row["value"])

		updated := false
		for i := range product.MetaData {
			if product.MetaData[i].Key != key {
				continue
			}
			product.MetaData[i].Value = value
			updated = true
			break
		}
		if !updated {
			product.MetaData = append(product.MetaData, wcMetaData{
				Key:   key,
				Value: value,
			})
		}
		if key == "_hurt_price" {
			product.HurtPrice = value
		}
	}
}
