// internal/syncer/syncer.go
package syncer

import (
	"context"
	"encoding/json"
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

type Syncer struct {
	log     zerolog.Logger // logowanie
	db      *gorm.DB       // dostęp do bazy
	mu      sync.Mutex     // ochrona sekcji krytycznych
	cfg     *conf.Config   // aktualna konfiguracja
	running bool           // czy syncer działa
	cancel  context.CancelFunc
	wg      sync.WaitGroup // śledzi goroutines
	ticks   uint64         // licznik heartbeatów
	ints    []runningInt   // lista aktywnych integracji
}

func New(log zerolog.Logger, cfg *conf.Config, gdb *gorm.DB) *Syncer {
	return &Syncer{log: log, cfg: cfg, db: gdb}
}

func (s *Syncer) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.running = true
	s.ticks = 0
	s.wg.Add(1)

	// zbuduj i odpal integracje
	ints := s.buildIntegrationsLocked()
	s.ints = ints
	s.mu.Unlock()

	s.log.Info().Msg("Syncer(dev): start")
	go s.loop(ctx)

	// każda integracja w swojej gorutinie
	for i := range ints {
		s.wg.Add(1)
		go func(intg integrations.Integration) {
			defer s.wg.Done()
			ctx = context.WithValue(ctx, "gormDB", s.db)
			if err := intg.Start(ctx); err != nil {
				s.log.Error().Err(err).Str("integration", intg.Name()).Msg("zakończona z błędem")
			}
		}(ints[i].Inst)
	}
	return nil
}

func (s *Syncer) buildIntegrationsLocked() []runningInt {
	var out []runningInt
	if s.cfg == nil || len(s.cfg.Integrations) == 0 {
		s.log.Warn().Msg("Integrations: brak lub puste (sprawdź config.json)")
		return out
	}
	s.log.Info().Int("count", len(s.cfg.Integrations)).Msg("Integrations in config")
	for name, raw := range s.cfg.Integrations {
		s.log.Info().Str("integration", name).RawJSON("raw", raw).Msg("Found integration in config")

		f, ok := integrations.Get(name)
		if !ok {
			s.log.Warn().Str("integration", name).Msg("brak fabryki – pomijam")
			continue
		}
		inst, err := f(s.log.With().Str("integration", name).Logger(), json.RawMessage(raw))
		if err != nil {
			s.log.Error().Err(err).Str("integration", name).Msg("błąd inicjalizacji")
			continue
		}
		out = append(out, runningInt{Name: name, Inst: inst})
	}
	s.log.Info().Int("started", len(out)).Msg("Integrations built")
	return out
}

func (s *Syncer) Stop() {
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
	s.mu.Unlock()

	for _, ri := range ints {
		ri.Inst.Stop()
	}
	if cancel != nil {
		cancel()
	}
	s.wg.Wait()
	s.log.Info().Msg("Syncer(dev): stop")
}

func (s *Syncer) UpdateConfig(cfg *conf.Config) {
	s.mu.Lock()
	s.cfg = cfg
	isRunning := s.running
	s.mu.Unlock()

	s.log.Info().Msg("Syncer(dev): config zaktualizowany")

	if isRunning {
		// szybki restart integracji, żeby wzięły nową konfigurację
		s.log.Info().Msg("Syncer(dev): restart integracji po zmianie configu")
		s.Stop()
		_ = s.Start(context.Background())
	}
}

func (s *Syncer) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
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

	ticker := time.NewTicker(s.interval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.log.Info().Msg("Syncer(dev): koniec pętli")
			return
		case <-ticker.C:
			// jeśli ktoś zmienił interwał w cfg — odśwież ticker
			newInt := s.interval()
			if newInt != tickerInterval(ticker) {
				ticker.Reset(newInt)
			}
			s.tickOnce()
		}
	}
}

func (s *Syncer) tickOnce() {
	s.mu.Lock()
	s.ticks++
	n := s.ticks
	s.mu.Unlock()

	// symulacja „pracy”
	// globalne taski lub health-checki integracji.
	s.log.Info().Msgf("Syncer(dev): heartbeat #%d (nic nie robię, tylko test)", n)
	time.Sleep(100 * time.Millisecond)
}

// pomocniczo: wyciągnij aktualny interwał z tickera (best-effort)
func tickerInterval(t *time.Ticker) time.Duration {
	// Go nie udostępnia tego oficjalnie – trzymamy lokalnie w pętli przez interval()
	// Tu zwracamy „bezpiecznik”, by zawsze umożliwić Reset na nową wartość.
	// W praktyce i tak porównamy z interval() i nadpiszemy.
	return 0
}
