package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	conf "github.com/bartek5186/pcm2www/internal/config"
	"github.com/bartek5186/pcm2www/internal/integrations"
	"github.com/rs/zerolog"
)

func TestStatusReflectsReadinessFailureAndHeartbeat(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		active, ready, blocked bool
		wooState               string
		heartbeatAge           time.Duration
		interval               int
		want                   StatusState
	}{
		{name: "stopped", want: StatusStopped},
		{name: "priming cache", active: true, wooState: "running", want: StatusStarting},
		{name: "integration starting", active: true, ready: true, wooState: "starting", want: StatusStarting},
		{name: "ready", active: true, ready: true, wooState: "running", want: StatusRunning},
		{name: "failure despite fresh heartbeat", active: true, wooState: "failed", want: StatusError},
		{name: "unexpected integration exit", active: true, ready: true, wooState: "stopped", want: StatusError},
		{name: "stale heartbeat", active: true, ready: true, wooState: "running", heartbeatAge: time.Minute, want: StatusError},
		{name: "configured slow heartbeat", active: true, ready: true, wooState: "running", heartbeatAge: time.Minute, interval: 60, want: StatusRunning},
		{name: "shutdown timed out", blocked: true, want: StatusError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(zerolog.Nop(), &conf.Config{SyncIntervalSeconds: tc.interval}, nil)
			s.running = tc.active
			s.shutdownBlocked = tc.blocked
			s.lastHeartbeat = time.Now().Add(-tc.heartbeatAge)
			s.runtime = integrations.NewRuntime(nil, !tc.ready)
			s.intStatus = map[string]IntegrationStatus{
				"importer":    {Name: "importer", State: "running"},
				"woocommerce": {Name: "woocommerce", State: tc.wooState, LastError: "prime cache: decode page 4"},
			}
			got := s.Status()
			if got.State != tc.want || got.Active != tc.active {
				t.Fatalf("wrong visible state: %+v", got)
			}
			if tc.wooState == "failed" && (!strings.Contains(got.Text, "woocommerce") || !strings.Contains(got.Detail, "decode page 4")) {
				t.Fatalf("failure details hidden: %+v", got)
			}
		})
	}
}

func TestStatusTracksStartCancelAndStop(t *testing.T) {
	const name = "status_lifecycle_test"
	started := make(chan context.Context, 1)
	integrations.Register(name, func(zerolog.Logger, json.RawMessage) (integrations.Integration, error) {
		return &lifecycleTestIntegration{started: started}, nil
	})
	s := New(zerolog.Nop(), &conf.Config{Integrations: map[string]json.RawMessage{name: json.RawMessage(`{}`)}}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if got := s.Status(); got.State != StatusStopped || got.Active {
		t.Fatalf("initial state: %+v", got)
	}
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	receiveStartedContext(t, started)
	if got := s.Status(); got.State != StatusRunning || !got.Active {
		t.Fatalf("started state: %+v", got)
	}
	cancel()
	if got := s.Status(); got.State != StatusStopped {
		t.Fatalf("canceled run must not stay green: %+v", got)
	}
	s.Stop()
	if got := s.Status(); got.State != StatusStopped || got.Active {
		t.Fatalf("stopped state: %+v", got)
	}
}

func TestHeartbeatUpdatesStateWithoutWritingLogs(t *testing.T) {
	var output bytes.Buffer
	s := New(zerolog.New(&output), &conf.Config{}, nil)
	before := time.Now()
	s.tickOnce()
	if s.lastHeartbeat.Before(before) {
		t.Fatal("heartbeat not updated")
	}
	if output.Len() != 0 {
		t.Fatalf("heartbeat still writes to log: %s", output.String())
	}
}
