package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	conf "github.com/bartek5186/pcm2www/internal/config"
	"github.com/bartek5186/pcm2www/internal/integrations"
	"github.com/rs/zerolog"
)

func TestBuildIntegrationsDoesNotLogRawConfigSecrets(t *testing.T) {
	const secret = "cs_must_not_appear_in_logs"
	raw, err := json.Marshal(map[string]any{
		"base_url":        "https://woo.test",
		"consumer_key":    "ck_test",
		"consumer_secret": secret,
	})
	if err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	logger := zerolog.New(&output)
	s := New(logger, &conf.Config{Integrations: map[string]json.RawMessage{
		"woocommerce": raw,
	}}, nil)
	if _, err := s.buildIntegrations(s.cfg); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(output.String(), secret) {
		t.Fatalf("integration secret leaked to logs: %s", output.String())
	}
}

func TestUpdateConfigKeepsOriginalParentContext(t *testing.T) {
	const integrationName = "syncer_lifecycle_test"
	started := make(chan context.Context, 2)
	integrations.Register(integrationName, func(zerolog.Logger, json.RawMessage) (integrations.Integration, error) {
		return &lifecycleTestIntegration{started: started}, nil
	})
	cfg := &conf.Config{Integrations: map[string]json.RawMessage{integrationName: json.RawMessage(`{}`)}}
	s := New(zerolog.Nop(), cfg, nil)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := s.Start(parent); err != nil {
		t.Fatal(err)
	}
	first := receiveStartedContext(t, started)
	if err := s.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	second := receiveStartedContext(t, started)
	if first == second {
		t.Fatal("config reload should create a new integration context")
	}

	cancel()
	select {
	case <-second.Done():
	case <-time.After(time.Second):
		t.Fatal("reloaded integration lost the original parent cancellation")
	}
	s.Stop()
	if s.IsRunning() {
		t.Fatal("syncer still reports running after stop")
	}
}

func TestStartRejectsCanceledContext(t *testing.T) {
	s := New(zerolog.Nop(), &conf.Config{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Start(ctx); err == nil {
		t.Fatal("expected canceled parent context to reject start")
	}
}

func TestUpdateConfigRejectsInvalidConfigWithoutStoppingCurrentRun(t *testing.T) {
	const integrationName = "syncer_transactional_reload_test"
	started := make(chan context.Context, 2)
	integrations.Register(integrationName, func(zerolog.Logger, json.RawMessage) (integrations.Integration, error) {
		return &lifecycleTestIntegration{started: started}, nil
	})
	cfg := &conf.Config{Integrations: map[string]json.RawMessage{integrationName: json.RawMessage(`{}`)}}
	s := New(zerolog.Nop(), cfg, nil)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	current := receiveStartedContext(t, started)

	bad := &conf.Config{Integrations: map[string]json.RawMessage{"does_not_exist": json.RawMessage(`{}`)}}
	if err := s.UpdateConfig(bad); err == nil {
		t.Fatal("expected invalid reload to be rejected")
	}
	select {
	case <-current.Done():
		t.Fatal("invalid reload stopped the working integration")
	default:
	}
	if !s.IsRunning() {
		t.Fatal("syncer stopped after rejected configuration")
	}
	s.Stop()
}

func TestIntegrationFailureIsVisibleInStatus(t *testing.T) {
	const integrationName = "syncer_failure_status_test"
	integrations.Register(integrationName, func(zerolog.Logger, json.RawMessage) (integrations.Integration, error) {
		return failingTestIntegration{}, nil
	})
	s := New(zerolog.Nop(), &conf.Config{Integrations: map[string]json.RawMessage{
		integrationName: json.RawMessage(`{}`),
	}}, nil)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		statuses := s.IntegrationStatuses()
		if len(statuses) == 1 && statuses[0].State == "failed" && strings.Contains(statuses[0].LastError, "forced") {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("integration failure not exposed: %+v", s.IntegrationStatuses())
}

type lifecycleTestIntegration struct {
	started chan<- context.Context
}

type failingTestIntegration struct{}

func (failingTestIntegration) Name() string { return "syncer_failure_status_test" }
func (failingTestIntegration) Start(context.Context) error {
	return fmt.Errorf("forced integration failure")
}
func (failingTestIntegration) Stop() {}

func (i *lifecycleTestIntegration) Name() string { return "syncer_lifecycle_test" }

func (i *lifecycleTestIntegration) Start(ctx context.Context) error {
	i.started <- ctx
	<-ctx.Done()
	return nil
}

func (*lifecycleTestIntegration) Stop() {}

func receiveStartedContext(t *testing.T, started <-chan context.Context) context.Context {
	t.Helper()
	select {
	case ctx := <-started:
		return ctx
	case <-time.After(time.Second):
		t.Fatal("integration did not start")
		return nil
	}
}
