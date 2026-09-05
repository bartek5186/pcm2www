package woocommerce

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/bartek5186/pcm2www/internal/db"
	"github.com/rs/zerolog"
)

func TestWorkerAppliesRequeuedPriceWithNewerRevisionAndOlderTaskID(t *testing.T) {
	gdb := newWooWorkerTestDB(t)
	wooID, sourceID := uint(900), int64(42)
	seedWorkerLink(t, gdb, wooID, sourceID, "5901234567890")
	payload, _ := json.Marshal(db.WooPriceUpdatePayload{WooID: wooID, TowarID: sourceID, DesiredRegular: 10, DesiredHurt: 8, DesiredTaxClass: "2300"})
	a := db.WooTask{TaskKey: "price:A", Kind: db.WooTaskKindPriceUpdate, WooID: &wooID, TowarID: &sourceID, PayloadJSON: string(payload), Status: "done", Revision: 1}
	b := db.WooTask{TaskKey: "price:B", Kind: db.WooTaskKindPriceUpdate, WooID: &wooID, TowarID: &sourceID, Status: "done", Revision: 2}
	if err := gdb.Create(&a).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&b).Error; err != nil {
		t.Fatal(err)
	}
	// Same update performed by enqueueWooTask on a return to an earlier price.
	if err := gdb.Model(&a).Updates(map[string]any{"status": "pending", "revision": 3}).Error; err != nil {
		t.Fatal(err)
	}
	state := map[uint]wcProduct{wooID: {ID: int64(wooID), GlobalUniqueID: "5901234567890", RegularPrice: "20", HurtPrice: "8", TaxClass: "2300", ManageStock: true}}
	client := newWooWorkerTestClient(t, state)
	reads, writes := 0, 0
	transport := client.Transport
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			reads++
		} else {
			writes++
		}
		return transport.RoundTrip(r)
	})
	w := &Woo{log: zerolog.Nop(), cfg: Config{BaseURL: "https://woo.test"}, http: client}
	w.workerTick(context.Background(), gdb)
	if parsePrice(state[wooID].RegularPrice) != 10 || writes != 1 || reads != 2 {
		t.Fatalf("returning price was not written and separately verified: product=%+v reads=%d writes=%d", state[wooID], reads, writes)
	}
	var current db.WooTask
	if err := gdb.First(&current, "task_id = ?", a.TaskID).Error; err != nil {
		t.Fatal(err)
	}
	if current.Status != "done" || current.Revision != 3 {
		t.Fatalf("wrong final task: %+v", current)
	}
	var cache db.WooProductCache
	if err := gdb.First(&cache, "woo_id = ?", wooID).Error; err != nil {
		t.Fatal(err)
	}
	if cache.PriceRegular != 10 {
		t.Fatalf("verified price not saved in cache: %+v", cache)
	}
}

func TestWorkerDoesNotWriteTaskSupersededDuringGET(t *testing.T) {
	for _, batch := range []bool{false, true} {
		name := "single"
		if batch {
			name = "batch"
		}
		t.Run(name, func(t *testing.T) {
			gdb := newWooWorkerTestDB(t)
			wooID, sourceID := uint(900), int64(42)
			seedWorkerLink(t, gdb, wooID, sourceID, "5901234567890")
			payload, _ := json.Marshal(db.WooPriceUpdatePayload{WooID: wooID, TowarID: sourceID, DesiredRegular: 20})
			task := db.WooTask{TaskKey: "stale-price", Kind: db.WooTaskKindPriceUpdate, WooID: &wooID, TowarID: &sourceID, PayloadJSON: string(payload), Status: "running", Revision: 1}
			if err := gdb.Create(&task).Error; err != nil {
				t.Fatal(err)
			}
			writes := 0
			w := &Woo{log: zerolog.Nop(), cfg: Config{BaseURL: "https://woo.test"}, http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet {
					writes++
					return textResponse(500, "must not write"), nil
				}
				if err := gdb.Model(&task).Update("status", "superseded").Error; err != nil {
					t.Fatal(err)
				}
				product := wcProduct{ID: int64(wooID), GlobalUniqueID: "5901234567890", RegularPrice: "10"}
				if batch {
					return jsonResponse(200, []wcProduct{product})
				}
				return jsonResponse(200, product)
			})}}
			if batch {
				w.executeBatch(context.Background(), gdb, task.Kind, []db.WooTask{task})
			} else {
				w.executeWooTask(context.Background(), gdb, task)
			}
			if writes != 0 {
				t.Fatal("superseded task reached write API")
			}
			var current db.WooTask
			if err := gdb.First(&current, "task_id = ?", task.TaskID).Error; err != nil {
				t.Fatal(err)
			}
			if current.Status != "superseded" {
				t.Fatalf("worker overwrote cancellation: %+v", current)
			}
		})
	}
}

func TestOldWorkerCannotClaimOrFinishReplannedRevision(t *testing.T) {
	for _, operation := range []string{"claim", "complete", "fail", "cancel", "retry"} {
		t.Run(operation, func(t *testing.T) {
			gdb := newWooWorkerTestDB(t)
			task := db.WooTask{TaskKey: "replanned", Kind: db.WooTaskKindPriceUpdate, Status: "pending", Revision: 1}
			if err := gdb.Create(&task).Error; err != nil {
				t.Fatal(err)
			}
			old := task
			if err := gdb.Model(&task).Update("revision", 2).Error; err != nil {
				t.Fatal(err)
			}
			var before db.WooTask
			if err := gdb.First(&before, "task_id = ?", task.TaskID).Error; err != nil {
				t.Fatal(err)
			}
			w := &Woo{log: zerolog.Nop()}
			// Assert after each operation independently: requeueing must not
			// conceal an earlier invalid completion or error update.
			switch operation {
			case "claim":
				claimed, err := claimWooTask(gdb, old)
				if err != nil || claimed != nil {
					t.Fatalf("old snapshot claimed new payload: %+v %v", claimed, err)
				}
			case "complete":
				w.completeWooTask(gdb, old, "done", "", "")
			case "fail":
				w.failWooTask(gdb, old, errors.New("late failure"))
			case "cancel":
				w.requeueWooTask(gdb, old, context.Canceled)
			case "retry":
				w.retryWooTask(gdb, old, errors.New("late transient failure"))
			}
			var current db.WooTask
			if err := gdb.First(&current, "task_id = ?", task.TaskID).Error; err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, current) {
				t.Fatalf("old worker modified new intent during %s: before=%+v after=%+v", operation, before, current)
			}
		})
	}
}
