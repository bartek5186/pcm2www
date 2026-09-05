package importer

import (
	"encoding/json"
	"testing"

	"github.com/bartek5186/pcm2www/internal/db"
)

func TestPriceModeUsesNetXMLPrices(t *testing.T) {
	for _, tc := range []struct {
		name, mode string
		net        float64
		vat        int64
		want       float64
	}{
		{"confirmed Finca price", "gross", 40.64, 2300, 49.99},
		{"default is gross", "", 40.64, 2300, 49.99},
		{"eight percent", "gross", 100, 800, 108},
		{"five percent", "gross", 100, 500, 105},
		{"zero rate", "gross", 40.64, 0, 40.64},
		{"exempt", "gross", 40.64, -1, 40.64},
		{"net unchanged", "net", 40.64, 2300, 40.64},
		{"zero price", "gross", 0, 2300, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			imp := &Importer{cfg: Config{PriceMode: tc.mode}}
			if got := imp.wooPriceFromNet(tc.net, tc.vat); got != tc.want {
				t.Fatalf("XML net=%v VAT=%d mode=%q: got %v want %v", tc.net, tc.vat, tc.mode, got, tc.want)
			}
		})
	}
}

func TestStartupCorrectsPreviouslyPlannedNetAsGrossPrice(t *testing.T) {
	for _, oldStatus := range []string{"pending", "done", "error"} {
		t.Run(oldStatus, func(t *testing.T) {
			imp, gdb := linkedPlanner(t)
			if err := gdb.Model(&db.StProduct{}).Where("towar_id = ?", 42).
				Updates(map[string]any{"cena_detal": 40.64, "cena_hurtowa": 10}).Error; err != nil {
				t.Fatal(err)
			}
			if err := gdb.Model(&db.WooProductCache{}).Where("woo_id = ?", 900).
				Updates(map[string]any{"price_regular": 40.64, "hurt_price": 10}).Error; err != nil {
				t.Fatal(err)
			}
			old := db.WooTask{ImportID: 1, TaskKey: buildTaskKey(db.WooTaskKindPriceUpdate, 900, normalizeFloatKey(40.64), normalizeFloatKey(10), "2300"), Kind: db.WooTaskKindPriceUpdate, WooID: ptrUint(900), TowarID: ptrInt64(42), Status: oldStatus, Revision: 1}
			if err := gdb.Create(&old).Error; err != nil {
				t.Fatal(err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				if err := imp.linkAndPlanCurrentStaging(); err != nil {
					t.Fatal(err)
				}
			}
			var tasks []db.WooTask
			if err := gdb.Where("kind = ? AND status = ?", db.WooTaskKindPriceUpdate, "pending").Find(&tasks).Error; err != nil {
				t.Fatal(err)
			}
			if len(tasks) != 1 {
				t.Fatalf("expected one corrective price task, got %+v", tasks)
			}
			var payload db.WooPriceUpdatePayload
			if err := json.Unmarshal([]byte(tasks[0].PayloadJSON), &payload); err != nil {
				t.Fatal(err)
			}
			if payload.DesiredRegular != 49.99 || payload.DesiredHurt != 12.3 || payload.DesiredTaxClass != "2300" {
				t.Fatalf("wrong corrective price: %+v", payload)
			}
			if oldStatus == "pending" {
				if err := gdb.First(&old, "task_id = ?", old.TaskID).Error; err != nil {
					t.Fatal(err)
				}
				if old.Status != "superseded" {
					t.Fatalf("old price still queued: %+v", old)
				}
			}
			// Once Woo confirms the gross prices, another startup must not
			// multiply the already-correct price or create another task.
			if err := gdb.Model(&tasks[0]).Update("status", "done").Error; err != nil {
				t.Fatal(err)
			}
			if err := gdb.Model(&db.WooProductCache{}).Where("woo_id = ?", 900).
				Updates(map[string]any{"price_regular": 49.99, "hurt_price": 12.3}).Error; err != nil {
				t.Fatal(err)
			}
			if err := imp.linkAndPlanCurrentStaging(); err != nil {
				t.Fatal(err)
			}
			var count int64
			if err := gdb.Model(&db.WooTask{}).Where("kind = ? AND status = ?", db.WooTaskKindPriceUpdate, "pending").Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("correct price queued again: %d", count)
			}
		})
	}
}
