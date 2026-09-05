package syncer

import (
	"sort"
	"time"
)

type StatusState string

const (
	StatusStopped  StatusState = "stopped"
	StatusStarting StatusState = "starting"
	StatusRunning  StatusState = "running"
	StatusError    StatusState = "error"
)

// StatusSnapshot is read atomically by the tray and the log window. Active
// controls Start/Stop; a run can remain active while an integration has failed.
type StatusSnapshot struct {
	State  StatusState
	Text   string
	Detail string
	Active bool
}

func (s *Syncer) Status() StatusSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := StatusSnapshot{State: StatusStopped, Text: "Synchronizacja zatrzymana", Detail: "Aplikacja jest otwarta. Uruchom synchronizację przyciskiem Start.", Active: s.running}
	if s.shutdownBlocked {
		status.State, status.Text, status.Detail = StatusError, "Błąd zatrzymania synchronizacji", "Operacja w tle nie zakończyła się w wymaganym czasie."
		return status
	}
	if !s.running || (s.parent != nil && s.parent.Err() != nil) {
		return status
	}
	names := make([]string, 0, len(s.intStatus))
	for name := range s.intStatus {
		names = append(names, name)
	}
	sort.Strings(names)
	// Failures take priority over another integration still starting.
	for _, name := range names {
		integration := s.intStatus[name]
		if integration.State == "failed" {
			status.State, status.Text, status.Detail = StatusError, "Błąd synchronizacji: "+name, name+": "+integration.LastError
			return status
		}
		if integration.State == "stopped" {
			status.State, status.Text, status.Detail = StatusError, "Integracja zatrzymana: "+name, "Zatrzymaj i uruchom synchronizację ponownie. Szczegóły znajdują się w logu."
			return status
		}
	}
	interval := 5 * time.Second
	if s.cfg != nil && s.cfg.SyncIntervalSeconds > 0 {
		interval = time.Duration(s.cfg.SyncIntervalSeconds) * time.Second
	}
	if !s.lastHeartbeat.IsZero() && time.Since(s.lastHeartbeat) > max(3*interval, 15*time.Second) {
		status.State, status.Text, status.Detail = StatusError, "Brak odpowiedzi synchronizacji", "Pętla synchronizacji nie potwierdziła działania w wymaganym czasie."
		return status
	}
	status.State, status.Text, status.Detail = StatusStarting, "Uruchamianie synchronizacji", "Oczekiwanie na uruchomienie integracji."
	if len(names) == 0 {
		return status
	}
	for _, name := range names {
		if s.intStatus[name].State != "running" {
			return status
		}
	}
	if s.runtime != nil && !s.runtime.IsWooCacheReady() {
		status.Detail = "Pobieranie danych WooCommerce. Importer oczekuje na gotowość sklepu."
		return status
	}
	status.State, status.Text, status.Detail = StatusRunning, "Synchronizacja działa", "Wszystkie integracje są uruchomione."
	return status
}
