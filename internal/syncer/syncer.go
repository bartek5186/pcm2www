// internal/syncer/syncer.go
package syncer

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"

	conf "github.com/bartek5186/pcm2www/internal/config"
	"github.com/bartek5186/pcm2www/internal/integrations" // + import rejestru/typów
	_ "github.com/bartek5186/pcm2www/internal/integrations/importer"
	_ "github.com/bartek5186/pcm2www/internal/integrations/woocommerce" // rejestracja
	"github.com/rs/zerolog"
	"gorm.io/gorm"
)

// wrapper na uruchomioną integrację (np. importer i woocommerce)
type runningInt struct {
	Name string
	Inst integrations.Integration
}

type IntegrationStatus struct {
	Name      string    `json:"name"`
	State     string    `json:"state"`
	LastError string    `json:"last_error,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
}

type Syncer struct {
	log             zerolog.Logger // logowanie
	db              *gorm.DB       // dostęp do bazy
	lifecycleMu     sync.Mutex     // serializuje Start/Stop/UpdateConfig
	mu              sync.Mutex     // ochrona stanu i konfiguracji
	cfg             *conf.Config   // aktualna konfiguracja
	running         bool           // czy syncer działa
	cancel          context.CancelFunc
	parent          context.Context
	wg              sync.WaitGroup // śledzi goroutines
	lastHeartbeat   time.Time      // ostatni przebieg pętli, bez wpisów w logu
	runtime         *integrations.Runtime
	ints            []runningInt // lista aktywnych integracji
	intStatus       map[string]IntegrationStatus
	shutdownBlocked bool
}

func New(log zerolog.Logger, cfg *conf.Config, gdb *gorm.DB) *Syncer {
	return &Syncer{log: log, cfg: cfg, db: gdb}
}

func (s *Syncer) Start(ctx context.Context) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	return s.startLocked(ctx)
}

func (s *Syncer) startLocked(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("syncer: nil context")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("syncer: start with canceled context: %w", err)
	}

	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return nil
	}
	if s.shutdownBlocked {
		s.mu.Unlock()
		return fmt.Errorf("syncer: poprzednie zatrzymanie nadal czeka na operację w tle")
	}
	cfg := s.cfg
	s.mu.Unlock()

	ints, err := s.buildIntegrations(cfg)
	if err != nil {
		return err
	}
	return s.startPreparedLocked(ctx, ints)
}

func (s *Syncer) startPreparedLocked(ctx context.Context, ints []runningInt) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("syncer: start with canceled context: %w", err)
	}
	waitsForWoo := false
	for _, ri := range ints {
		if ri.Name == "woocommerce" {
			waitsForWoo = true
			break
		}
	}
	parent := ctx
	runCtx, cancel := context.WithCancel(parent)
	runtime := integrations.NewRuntime(s.db, waitsForWoo)
	runCtx = integrations.WithRuntime(runCtx, runtime)
	now := time.Now()
	statuses := make(map[string]IntegrationStatus, len(ints))
	for _, ri := range ints {
		statuses[ri.Name] = IntegrationStatus{Name: ri.Name, State: "starting", StartedAt: now}
	}

	s.mu.Lock()
	s.cancel = cancel
	s.parent = parent
	s.running = true
	s.lastHeartbeat = now
	s.runtime = runtime
	s.ints = ints
	s.intStatus = statuses
	s.wg.Add(1 + len(ints))
	s.mu.Unlock()

	s.log.Info().Int("integrations", len(ints)).Msg("syncer started")
	go s.loop(runCtx)

	// każda integracja w swojej gorutinie
	for i := range ints {
		go func(ri runningInt) {
			defer s.wg.Done()
			s.setIntegrationState(ri.Name, "running", "")
			if err := ri.Inst.Start(runCtx); err != nil {
				s.setIntegrationState(ri.Name, "failed", err.Error())
				s.log.Error().Err(err).Str("integration", ri.Name).Msg("integration stopped with error")
				return
			}
			s.setIntegrationState(ri.Name, "stopped", "")
		}(ints[i])
	}
	return nil
}

func (s *Syncer) buildIntegrations(cfg *conf.Config) ([]runningInt, error) {
	var out []runningInt
	if cfg == nil {
		return nil, fmt.Errorf("syncer: nil config")
	}
	if len(cfg.Integrations) == 0 {
		return nil, fmt.Errorf("syncer: no integrations configured")
	}
	names := make([]string, 0, len(cfg.Integrations))
	for name := range cfg.Integrations {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		raw := cfg.Integrations[name]

		f, ok := integrations.Get(name)
		if !ok {
			return nil, fmt.Errorf("syncer: unknown integration %q", name)
		}
		inst, err := f(s.log.With().Str("integration", name).Logger(), json.RawMessage(raw))
		if err != nil {
			return nil, fmt.Errorf("syncer: initialize integration %q: %w", name, err)
		}
		out = append(out, runningInt{Name: name, Inst: inst})
	}
	return out, nil
}

func (s *Syncer) setIntegrationState(name, state, lastError string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := s.intStatus[name]
	status.Name = name
	status.State = state
	status.LastError = lastError
	s.intStatus[name] = status
}

func (s *Syncer) Stop() {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.stopLocked()
}

func (s *Syncer) stopLocked() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.running = false
	cancel := s.cancel
	ints := s.ints
	s.ints = nil
	s.cancel = nil
	s.parent = nil
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	for _, ri := range ints {
		ri.Inst.Stop()
	}
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		s.mu.Lock()
		s.shutdownBlocked = false
		s.mu.Unlock()
	case <-time.After(30 * time.Second):
		s.mu.Lock()
		s.shutdownBlocked = true
		s.mu.Unlock()
		go func() {
			<-done
			s.mu.Lock()
			s.shutdownBlocked = false
			s.mu.Unlock()
		}()
		s.log.Error().Msg("syncer shutdown deadline exceeded; background operation did not stop")
	}
	s.log.Info().Msg("syncer stopped")
}

func (s *Syncer) UpdateConfig(cfg *conf.Config) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if cfg == nil {
		return fmt.Errorf("syncer: nil config")
	}

	s.mu.Lock()
	isRunning := s.running
	parent := s.parent
	oldCfg := s.cfg
	s.mu.Unlock()

	if isRunning && oldCfg != nil && !reflect.DeepEqual(oldCfg.Database, cfg.Database) {
		return fmt.Errorf("syncer: zmiana konfiguracji bazy wymaga restartu aplikacji")
	}
	ints, err := s.buildIntegrations(cfg)
	if err != nil {
		return err
	}
	if isRunning && (parent == nil || parent.Err() != nil) {
		return fmt.Errorf("syncer: cannot reload using canceled parent context")
	}
	if !isRunning {
		s.mu.Lock()
		s.cfg = cfg
		s.mu.Unlock()
		return nil
	}

	s.stopLocked()
	s.mu.Lock()
	shutdownBlocked := s.shutdownBlocked
	s.mu.Unlock()
	if shutdownBlocked {
		return fmt.Errorf("syncer: config reload aborted because previous integrations missed shutdown deadline")
	}
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
	if err := s.startPreparedLocked(parent, ints); err != nil {
		s.mu.Lock()
		s.cfg = oldCfg
		s.mu.Unlock()
		return fmt.Errorf("syncer: restart after config update: %w", err)
	}
	s.log.Info().Msg("syncer configuration reloaded")
	return nil
}

// ValidateConfig performs the same integration construction checks as Start,
// without changing the current configuration or lifecycle state.
func (s *Syncer) ValidateConfig(cfg *conf.Config) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if cfg == nil {
		return fmt.Errorf("syncer: nil config")
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	_, err := s.buildIntegrations(cfg)
	return err
}

func (s *Syncer) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

func (s *Syncer) IntegrationStatuses() []IntegrationStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]IntegrationStatus, 0, len(s.intStatus))
	for _, status := range s.intStatus {
		out = append(out, status)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

func (s *Syncer) interval() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg != nil && s.cfg.SyncIntervalSeconds > 0 {
		return time.Duration(s.cfg.SyncIntervalSeconds) * time.Second
	}
	return 5 * time.Second // krótszy interwał do dev
}

func (s *Syncer) loop(ctx context.Context) {
	defer s.wg.Done()

	// pierwszy strzał od razu
	s.tickOnce()

	currentInterval := s.interval()
	ticker := time.NewTicker(currentInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.log.Debug().Msg("syncer loop stopped")
			return
		case <-ticker.C:
			// jeśli ktoś zmienił interwał w cfg — odśwież ticker
			newInt := s.interval()
			if newInt != currentInterval {
				ticker.Reset(newInt)
				currentInterval = newInt
			}
			s.tickOnce()
		}
	}
}

func (s *Syncer) tickOnce() {
	s.mu.Lock()
	s.lastHeartbeat = time.Now()
	s.mu.Unlock()
}
