package appsettings

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	conf "github.com/bartek5186/pcm2www/internal/config"
)

type fakeRuntime struct {
	running                bool
	validations, updates   int
	validateErr, updateErr error
}

func (f *fakeRuntime) IsRunning() bool                   { return f.running }
func (f *fakeRuntime) ValidateConfig(*conf.Config) error { f.validations++; return f.validateErr }
func (f *fakeRuntime) UpdateConfig(*conf.Config) error   { f.updates++; return f.updateErr }

func TestSaveDraftAndRejectInvalidLiveReplacement(t *testing.T) {
	for _, tc := range []struct {
		name                string
		running, failUpdate bool
	}{{"stopped draft", false, false}, {"invalid live replacement", true, false}, {"apply error restores file", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			old := &conf.Config{Database: conf.DBConfig{Driver: "sqlite"}, SyncIntervalSeconds: 5}
			candidate := &conf.Config{Database: old.Database, SyncIntervalSeconds: 7, Integrations: map[string]json.RawMessage{"woocommerce": json.RawMessage(`{"enabled":false,"base_url":"https://shop.example"}`)}}
			if err := conf.Save(path, old); err != nil {
				t.Fatal(err)
			}
			f := &fakeRuntime{running: tc.running, validateErr: errors.New("missing key")}
			if tc.failUpdate {
				f.updateErr = errors.New("reload failed")
			}
			err := SaveAndApply(path, old, candidate, f)
			if (err != nil) != (tc.running || tc.failUpdate) {
				t.Fatalf("unexpected save result: %v", err)
			}
			if !tc.running && f.validations != 0 {
				t.Fatal("draft required runnable credentials")
			}
			if tc.running && f.updates != 0 {
				t.Fatal("invalid replacement reached runtime")
			}
			loaded, _, loadErr := conf.LoadOrCreate(path)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			want := candidate.SyncIntervalSeconds
			if err != nil {
				want = old.SyncIntervalSeconds
			}
			if loaded.SyncIntervalSeconds != want {
				t.Fatalf("wrong saved config: %+v", loaded)
			}
		})
	}
}
