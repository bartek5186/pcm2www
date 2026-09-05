package woocommerce

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bartek5186/pcm2www/internal/db"
	"github.com/glebarez/sqlite"
	"github.com/rs/zerolog"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func TestWorkerTickAppliesAndVerifiesTasks(t *testing.T) {
	state := map[uint]wcProduct{
		10: {
			ID:             10,
			Name:           "Worker Product",
			SKU:            "SKU-10",
			GlobalUniqueID: "5901234567890",
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
		Ean:          "5901234567890",
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
	seedWorkerSource(t, gdb, towarID, "5901234567890")

	stockPayload, _ := json.Marshal(db.WooStockUpdatePayload{ImportID: 1, WooID: wooID, TowarID: towarID, DesiredStock: 4})
	pricePayload, _ := json.Marshal(db.WooPriceUpdatePayload{ImportID: 1, WooID: wooID, TowarID: towarID, DesiredRegular: 25, DesiredHurt: 17})

	if err := gdb.Create([]db.WooTask{
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

func TestLegacyEANUpdateTaskIsSkippedWithoutWooRequest(t *testing.T) {
	gdb := newWooWorkerTestDB(t)
	importID := uint(2)
	wooID := uint(10)
	towarID := int64(201)
	if err := gdb.Create(&db.ImportFile{ImportID: importID, Filename: "exp_wyk_legacy_ean.xml", Status: 1}).Error; err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(db.WooEANUpdatePayload{
		ImportID: importID, WooID: wooID, TowarID: towarID, DesiredEAN: "5909999999999",
	})
	if err := gdb.Create(&db.WooTask{
		TaskKey: "ean.update:10:5909999999999", ImportID: importID, TowarID: &towarID, WooID: &wooID,
		Kind: db.WooTaskKindEANUpdate, PayloadJSON: string(payload), Status: "pending",
	}).Error; err != nil {
		t.Fatal(err)
	}

	w := &Woo{
		log: zerolog.Nop(),
		http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("legacy EAN task must not call WooCommerce")
			return nil, nil
		})},
	}
	w.workerTick(context.Background(), gdb)

	var task db.WooTask
	if err := gdb.First(&task).Error; err != nil {
		t.Fatal(err)
	}
	if task.Status != "skipped" || !strings.Contains(task.LastError, "EAN updates disabled") {
		t.Fatalf("expected skipped legacy EAN task, got %+v", task)
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
			ID:             11,
			Name:           "No stock manage",
			SKU:            "SKU-11",
			GlobalUniqueID: "2222222222222",
			RegularPrice:   "20",
			SalePrice:      "0",
			HurtPrice:      "10",
			ManageStock:    false,
			StockQuantity:  1,
			Status:         "publish",
			Type:           "simple",
		},
		12: {
			ID:             12,
			Name:           "Sale active",
			SKU:            "SKU-12",
			GlobalUniqueID: "3333333333333",
			RegularPrice:   "20",
			SalePrice:      "5",
			HurtPrice:      "10",
			ManageStock:    true,
			StockQuantity:  1,
			Status:         "publish",
			Type:           "simple",
		},
	}

	client := newWooWorkerTestClient(t, state)

	gdb := newWooWorkerTestDB(t)
	importID := uint(2)
	if err := gdb.Create(&db.ImportFile{ImportID: importID, Filename: "exp_wyk_policy.xml", Status: 1}).Error; err != nil {
		t.Fatal(err)
	}

	towarB, towarC := int64(202), int64(203)
	wooB, wooC := uint(11), uint(12)
	if err := gdb.Create([]db.WooProductCache{
		{WooID: wooB, TowarID: &towarB, Kod: "SKU-11", Ean: "2222222222222", Name: "No stock manage", StockManaged: false, StockQty: 1, PriceRegular: 20, HurtPrice: 10},
		{WooID: wooC, TowarID: &towarC, Kod: "SKU-12", Ean: "3333333333333", Name: "Sale active", StockManaged: true, StockQty: 1, PriceRegular: 20, PriceSale: 5, HurtPrice: 10},
	}).Error; err != nil {
		t.Fatal(err)
	}
	seedWorkerSource(t, gdb, towarB, "2222222222222")
	seedWorkerSource(t, gdb, towarC, "3333333333333")

	stockPayload, _ := json.Marshal(db.WooStockUpdatePayload{ImportID: importID, WooID: wooB, TowarID: towarB, DesiredStock: 4})
	pricePayload, _ := json.Marshal(db.WooPriceUpdatePayload{ImportID: importID, WooID: wooC, TowarID: towarC, DesiredRegular: 25, DesiredHurt: 17})

	if err := gdb.Create([]db.WooTask{
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
		db.WooTaskKindStockUpdate: "skipped",
		db.WooTaskKindPriceUpdate: "skipped",
	}
	for _, row := range rows {
		if want[row.Kind] != row.Status {
			t.Fatalf("unexpected status for %s: got %s want %s", row.Kind, row.Status, want[row.Kind])
		}
	}
}

func TestAvailabilityReactivationRestoresStock(t *testing.T) {
	state := map[uint]wcProduct{
		20: {
			ID:                20,
			Name:              "Reactivated",
			GlobalUniqueID:    "4444444444444",
			ManageStock:       false,
			StockQuantity:     0,
			StockStatus:       "outofstock",
			Backorders:        "no",
			CatalogVisibility: "hidden",
		},
	}
	gdb := newWooWorkerTestDB(t)
	importID := uint(3)
	towarID := int64(301)
	wooID := uint(20)
	seedWorkerLink(t, gdb, wooID, towarID, "4444444444444")
	payload, _ := json.Marshal(db.WooAvailabilityPayload{
		ImportID: importID, WooID: wooID, TowarID: towarID,
		Unavailable: false, SetStock: true, DesiredStock: 6,
	})
	if err := gdb.Create(&db.WooTask{
		TaskKey: "availability.update:20:available:6", ImportID: importID, TowarID: &towarID, WooID: &wooID,
		Kind: db.WooTaskKindAvailabilityUpdate, PayloadJSON: string(payload), Status: "pending",
	}).Error; err != nil {
		t.Fatal(err)
	}

	w := &Woo{log: zerolog.Nop(), cfg: Config{BaseURL: "https://woo.test"}, http: newWooWorkerTestClient(t, state)}
	w.workerTick(context.Background(), gdb)

	product := state[wooID]
	if !product.ManageStock || product.StockQuantity != 6 || product.Backorders != "notify" || product.CatalogVisibility != "visible" {
		t.Fatalf("reactivation did not restore availability and stock: %+v", product)
	}
	var task db.WooTask
	if err := gdb.Where("task_key = ?", "availability.update:20:available:6").Take(&task).Error; err != nil {
		t.Fatal(err)
	}
	if task.Status != "done" {
		t.Fatalf("expected done reactivation task, got %+v", task)
	}
}

func TestWorkerTickRetriesTransientBatchFailure(t *testing.T) {
	gdb := newWooWorkerTestDB(t)
	importID := uint(5)
	towarID := int64(501)
	wooID := uint(50)

	if err := gdb.Create(&db.ImportFile{ImportID: importID, Filename: "exp_wyk_batch_failure.xml", Status: 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&db.WooProductCache{
		WooID:        wooID,
		TowarID:      &towarID,
		Kod:          "SKU-50",
		Ean:          "5555555555555",
		Name:         "Batch Failure Product",
		PriceRegular: 20,
		HurtPrice:    10,
		StockManaged: true,
	}).Error; err != nil {
		t.Fatal(err)
	}
	seedWorkerSource(t, gdb, towarID, "5555555555555")

	payload, _ := json.Marshal(db.WooPriceUpdatePayload{
		ImportID:       importID,
		WooID:          wooID,
		TowarID:        towarID,
		SKU:            "SKU-50",
		ProductName:    "Batch Failure Product",
		DesiredRegular: 25,
		DesiredHurt:    17,
	})
	if err := gdb.Create(&db.WooTask{
		TaskKey:     "price.update:50:25:17",
		ImportID:    importID,
		TowarID:     &towarID,
		WooID:       &wooID,
		Kind:        db.WooTaskKindPriceUpdate,
		PayloadJSON: string(payload),
		Status:      "pending",
	}).Error; err != nil {
		t.Fatal(err)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/wp-json/wc/v3/products":
			_ = json.NewEncoder(w).Encode([]wcProduct{{
				ID:             int64(wooID),
				Name:           "Batch Failure Product",
				SKU:            "SKU-50",
				GlobalUniqueID: "5555555555555",
				RegularPrice:   "20",
				SalePrice:      "0",
				MetaData:       []wcMetaData{{Key: "_hurt_price", Value: "10"}},
				ManageStock:    true,
				StockQuantity:  4,
				Status:         "publish",
				Type:           "simple",
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/wp-json/wc/v3/products/batch":
			http.Error(w, `{"code":"woocommerce_rest_cannot_update","message":"forced failure"}`, http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	})
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		return rec.Result(), nil
	})}

	w := &Woo{
		log:  zerolog.Nop(),
		cfg:  Config{BaseURL: "https://woo.test", ConsumerKey: "ck", ConsumerSec: "cs"},
		http: client,
	}

	w.workerTick(context.Background(), gdb)

	var task db.WooTask
	if err := gdb.Where("task_key = ?", "price.update:50:25:17").Take(&task).Error; err != nil {
		t.Fatal(err)
	}
	if task.Status != "pending" || task.NextAttemptAt == nil {
		t.Fatalf("expected batch POST failure to schedule retry, got %+v", task)
	}
	if !strings.Contains(task.LastError, "batch POST") || !strings.Contains(task.LastError, "http 500") {
		t.Fatalf("expected last_error to mention batch POST http 500, got %q", task.LastError)
	}
	if task.Attempts != 1 {
		t.Fatalf("expected one attempt, got %+v", task)
	}

	var cache db.WooProductCache
	if err := gdb.Where("woo_id = ?", wooID).Take(&cache).Error; err != nil {
		t.Fatal(err)
	}
	if cache.PriceRegular != 20 || cache.HurtPrice != 10 {
		t.Fatalf("cache should not pretend failed price update succeeded: %+v", cache)
	}
}

func TestTransientFailureStopsAfterMaximumAttempts(t *testing.T) {
	gdb := newWooWorkerTestDB(t)
	importID := uint(6)
	towarID := int64(601)
	wooID := uint(60)
	seedWorkerLink(t, gdb, wooID, towarID, "6666666666666")
	if err := gdb.Create(&db.ImportFile{ImportID: importID, Filename: "exp_wyk_retry_limit.xml", Status: 1}).Error; err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(db.WooStockUpdatePayload{ImportID: importID, WooID: wooID, TowarID: towarID, DesiredStock: 4})
	if err := gdb.Create(&db.WooTask{
		TaskKey: "stock.update:60:4", ImportID: importID, TowarID: &towarID, WooID: &wooID,
		Kind: db.WooTaskKindStockUpdate, PayloadJSON: string(payload), Status: "pending", Attempts: maxWooTaskAttempts - 1,
	}).Error; err != nil {
		t.Fatal(err)
	}

	w := &Woo{
		log: zerolog.Nop(),
		cfg: Config{BaseURL: "https://woo.test"},
		http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return textResponse(http.StatusServiceUnavailable, "try later"), nil
		})},
	}
	w.workerTick(context.Background(), gdb)

	var task db.WooTask
	if err := gdb.Where("task_key = ?", "stock.update:60:4").Take(&task).Error; err != nil {
		t.Fatal(err)
	}
	if task.Status != "error" || task.Attempts != maxWooTaskAttempts || task.NextAttemptAt != nil {
		t.Fatalf("expected terminal error after retry limit, got %+v", task)
	}
}

func TestRecoverRunningWooTasks(t *testing.T) {
	gdb := newWooWorkerTestDB(t)
	started := time.Now().Add(-time.Minute)
	if err := gdb.Create(&db.WooTask{TaskKey: "stuck", Kind: db.WooTaskKindStockUpdate, Status: "running", StartedAt: &started}).Error; err != nil {
		t.Fatal(err)
	}

	count, err := recoverRunningWooTasks(gdb)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected one recovered task, got %d", count)
	}
	var task db.WooTask
	if err := gdb.Where("task_key = ?", "stuck").Take(&task).Error; err != nil {
		t.Fatal(err)
	}
	if task.Status != "pending" || task.StartedAt != nil {
		t.Fatalf("expected recovered pending task, got %+v", task)
	}
}

func TestExecuteWooTaskSkipsObsoleteDesiredState(t *testing.T) {
	gdb := newWooWorkerTestDB(t)
	towarID := int64(701)
	wooID := uint(70)
	oldPayload, _ := json.Marshal(db.WooStockUpdatePayload{WooID: wooID, TowarID: towarID, DesiredStock: 1})
	newPayload, _ := json.Marshal(db.WooStockUpdatePayload{WooID: wooID, TowarID: towarID, DesiredStock: 2})
	old := db.WooTask{
		TaskKey: "stock.update:70:1", TowarID: &towarID, WooID: &wooID,
		Kind: db.WooTaskKindStockUpdate, PayloadJSON: string(oldPayload), Status: "running", Attempts: 1,
	}
	if err := gdb.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&db.WooTask{
		TaskKey: "stock.update:70:2", TowarID: &towarID, WooID: &wooID,
		Kind: db.WooTaskKindStockUpdate, PayloadJSON: string(newPayload), Status: "pending",
	}).Error; err != nil {
		t.Fatal(err)
	}

	called := false
	w := &Woo{
		log: zerolog.Nop(), cfg: Config{BaseURL: "https://woo.test"},
		http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			called = true
			return textResponse(http.StatusInternalServerError, "must not be called"), nil
		})},
	}
	w.executeWooTask(context.Background(), gdb, old)

	if called {
		t.Fatal("obsolete task reached Woo API")
	}
	var refreshed db.WooTask
	if err := gdb.Where("task_id = ?", old.TaskID).Take(&refreshed).Error; err != nil {
		t.Fatal(err)
	}
	if refreshed.Status != "superseded" {
		t.Fatalf("expected obsolete task to be superseded, got %+v", refreshed)
	}
}

func TestExecuteWooTaskSupersedesChangedEANLinkBeforeWooRequest(t *testing.T) {
	gdb := newWooWorkerTestDB(t)
	towarID := int64(702)
	wooID := uint(71)
	seedWorkerSource(t, gdb, towarID, "5900000000002")
	if err := gdb.Create(&db.WooProductCache{
		WooID: wooID, TowarID: &towarID, Ean: "5900000000001", StockManaged: true,
	}).Error; err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(db.WooStockUpdatePayload{WooID: wooID, TowarID: towarID, DesiredStock: 9})
	task := db.WooTask{
		TaskKey: "stock.update:71:9", TowarID: &towarID, WooID: &wooID,
		Kind: db.WooTaskKindStockUpdate, PayloadJSON: string(payload), Status: "running", Attempts: 1,
	}
	if err := gdb.Create(&task).Error; err != nil {
		t.Fatal(err)
	}

	called := false
	w := &Woo{log: zerolog.Nop(), cfg: Config{BaseURL: "https://woo.test"}, http: &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			called = true
			return textResponse(http.StatusInternalServerError, "must not be called"), nil
		}),
	}}
	w.executeWooTask(context.Background(), gdb, task)

	if called {
		t.Fatal("task with changed EAN link reached Woo API")
	}
	var refreshed db.WooTask
	if err := gdb.Where("task_id = ?", task.TaskID).Take(&refreshed).Error; err != nil {
		t.Fatal(err)
	}
	if refreshed.Status != "superseded" || !strings.Contains(refreshed.LastError, "EAN link changed") {
		t.Fatalf("expected stale EAN task to be superseded, got %+v", refreshed)
	}
}

func TestExecuteWooTaskSupersedesWhenLiveWooEANChanged(t *testing.T) {
	gdb := newWooWorkerTestDB(t)
	towarID := int64(703)
	wooID := uint(72)
	seedWorkerLink(t, gdb, wooID, towarID, "5900000000003")
	payload, _ := json.Marshal(db.WooStockUpdatePayload{WooID: wooID, TowarID: towarID, DesiredStock: 9})
	task := db.WooTask{
		TaskKey: "stock.update:72:9", TowarID: &towarID, WooID: &wooID,
		Kind: db.WooTaskKindStockUpdate, PayloadJSON: string(payload), Status: "running", Attempts: 1,
	}
	if err := gdb.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	state := map[uint]wcProduct{wooID: {
		ID: int64(wooID), GlobalUniqueID: "5900000000099", ManageStock: true, StockQuantity: 2,
	}}
	w := &Woo{log: zerolog.Nop(), cfg: Config{BaseURL: "https://woo.test"}, http: newWooWorkerTestClient(t, state)}
	w.executeWooTask(context.Background(), gdb, task)

	if state[wooID].StockQuantity != 2 {
		t.Fatalf("task updated product after live EAN changed: %+v", state[wooID])
	}
	var refreshed db.WooTask
	if err := gdb.Where("task_id = ?", task.TaskID).Take(&refreshed).Error; err != nil {
		t.Fatal(err)
	}
	if refreshed.Status != "superseded" || !strings.Contains(refreshed.LastError, "live Woo EAN") {
		t.Fatalf("expected live-EAN task to be superseded, got %+v", refreshed)
	}
	var cache db.WooProductCache
	if err := gdb.Where("woo_id = ?", wooID).Take(&cache).Error; err != nil {
		t.Fatal(err)
	}
	if cache.TowarID != nil || cache.Ean != "5900000000099" {
		t.Fatalf("live EAN change was not reflected in cache: %+v", cache)
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
	seedWorkerLink(t, gdb, wooID, towarID, "7777777777777")

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

	task, err := claimOneWooTaskOfKind(gdb, db.WooTaskKindStockUpdate)
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
	sqlDB, err := gdb.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close test DB: %v", err)
		}
	})
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

func seedWorkerSource(t *testing.T, gdb *gorm.DB, towarID int64, ean string) {
	t.Helper()
	if err := gdb.Create(&db.StProduct{TowarID: towarID, Kod: ean, CenaDetal: 1}).Error; err != nil {
		t.Fatal(err)
	}
}

func seedWorkerLink(t *testing.T, gdb *gorm.DB, wooID uint, towarID int64, ean string) {
	t.Helper()
	seedWorkerSource(t, gdb, towarID, ean)
	if err := gdb.Create(&db.WooProductCache{WooID: wooID, TowarID: &towarID, Ean: ean, StockManaged: true}).Error; err != nil {
		t.Fatal(err)
	}
}

func newWooWorkerTestClient(t *testing.T, state map[uint]wcProduct) *http.Client {
	t.Helper()

	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		path := strings.TrimPrefix(r.URL.Path, "/wp-json/wc/v3/products")
		path = strings.Trim(path, "/")
		if path == "batch" {
			if r.Method != http.MethodPost {
				return textResponse(http.StatusMethodNotAllowed, "method not allowed"), nil
			}

			var body struct {
				Update []map[string]any `json:"update"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				return textResponse(http.StatusBadRequest, "bad json"), nil
			}

			updated := make([]wcProduct, 0, len(body.Update))
			for _, item := range body.Update {
				id64, ok := toUint64(item["id"])
				if !ok {
					return textResponse(http.StatusBadRequest, "bad id"), nil
				}
				id := uint(id64)
				product, exists := state[id]
				if !exists {
					return textResponse(http.StatusNotFound, "not found"), nil
				}
				applyProductUpdate(&product, item)
				state[id] = product
				updated = append(updated, product)
			}

			return jsonResponse(http.StatusOK, map[string]any{"update": updated})
		}

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
			applyProductUpdate(&product, body)
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

func applyProductUpdate(product *wcProduct, body map[string]any) {
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
	if raw, ok := body["tax_class"]; ok {
		product.TaxClass = fmt.Sprint(raw)
	}
	if raw, ok := body["manage_stock"]; ok {
		if b, ok := raw.(bool); ok {
			product.ManageStock = b
		}
	}
	if raw, ok := body["stock_status"]; ok {
		product.StockStatus = fmt.Sprint(raw)
	}
	if raw, ok := body["backorders"]; ok {
		product.Backorders = fmt.Sprint(raw)
	}
	if raw, ok := body["catalog_visibility"]; ok {
		product.CatalogVisibility = fmt.Sprint(raw)
	}
	if raw, ok := body["meta_data"]; ok {
		applyMetaDataUpdate(product, raw)
	}
}

func toUint64(v any) (uint64, bool) {
	switch x := v.(type) {
	case float64:
		return uint64(x), true
	case int:
		return uint64(x), true
	case int64:
		return uint64(x), true
	case uint:
		return uint64(x), true
	case uint64:
		return x, true
	case string:
		n, err := strconv.ParseUint(x, 10, 64)
		return n, err == nil
	default:
		return 0, false
	}
}
