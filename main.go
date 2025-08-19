package main

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/getlantern/systray"

	import "github.com/bartek5186/pcm2www/internal"
)

// Uwaga: ścieżka w go:embed jest względna względem TEGO pliku.
// Jeśli trzymasz icon.ico w repo pod /assets/icon.ico i ten plik jest w cmd/tray/,
// to poniższa ścieżka jest poprawna.
//
//go:embed ../../assets/icon.ico
var iconData []byte

// wersję możesz nadpisać przez: -ldflags "-X 'main.ver=1.0.1'"
var ver = "1.0.0"

func main() {
	// katalog danych aplikacji (logi, config)
	appDir := mustAppDataDir("pcm2www")
	log := logs.New(filepath.Join(appDir, "app.log"))

	cfgPath := filepath.Join(appDir, "config.json")
	cfg, firstRun, err := conf.LoadOrCreate(cfgPath)
	if err != nil {
		panic(err)
	}
	if firstRun {
		log.Infof("Utworzono domyślną konfigurację: %s", cfgPath)
	}

	// kontekst sterujący życiem procesu (CTRL+C / zamknięcie sesji)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	s := syncer.New(log, cfg)

	// jeśli proces dostanie sygnał – zatrzymaj syncer i zamknij tray
	go func() {
		<-ctx.Done()
		s.Stop()
		systray.Quit()
	}()

	systray.Run(func() {
		// onReady
		if len(iconData) > 0 {
			systray.SetIcon(iconData)
		}
		systray.SetTooltip(fmt.Sprintf("PCM2WWW Sync %s", ver))

		mStart := systray.AddMenuItem("Start synchronizacji", "Uruchom harmonogram")
		mStop := systray.AddMenuItem("Stop synchronizacji", "Zatrzymaj harmonogram")
		mStop.Disable()

		systray.AddSeparator()
		mOpenLogs := systray.AddMenuItem("Otwórz logi", "Pokaż plik log")
		mOpenCfg := systray.AddMenuItem("Ustawienia (config.json)", "Otwórz plik konfiguracyjny")
		mReload := systray.AddMenuItem("Przeładuj konfigurację", "Wczytaj ponownie config.json")
		systray.AddSeparator()
		mAbout := systray.AddMenuItem(fmt.Sprintf("O programie (%s)", ver), "")
		mQuit := systray.AddMenuItem("Wyjście", "Zamknij aplikację")

		// AutoStart harmonogramu (nie mylić z autostartem Windows!)
		if cfg.AutoStart {
			if err := s.Start(ctx); err == nil {
				mStart.Disable()
				mStop.Enable()
				systray.SetTooltip(fmt.Sprintf("PCM2WWW Sync %s — działa", ver))
			} else {
				log.Errorf("AutoStart nieudany: %v", err)
				systray.SetTooltip(fmt.Sprintf("PCM2WWW Sync %s — błąd startu", ver))
			}
		}

		go func() {
			for {
				select {
				case <-mStart.ClickedCh:
					if err := s.Start(ctx); err != nil {
						log.Errorf("Start error: %v", err)
						systray.SetTooltip(fmt.Sprintf("PCM2WWW Sync %s — błąd startu", ver))
						continue
					}
					mStart.Disable()
					mStop.Enable()
					systray.SetTooltip(fmt.Sprintf("PCM2WWW Sync %s — działa", ver))

				case <-mStop.ClickedCh:
					s.Stop()
					mStop.Disable()
					mStart.Enable()
					systray.SetTooltip(fmt.Sprintf("PCM2WWW Sync %s — zatrzymane", ver))

				case <-mOpenLogs.ClickedCh:
					openInExplorer(filepath.Join(appDir, "app.log"))

				case <-mOpenCfg.ClickedCh:
					openInExplorer(cfgPath)

				case <-mReload.ClickedCh:
					newCfg, _, err := conf.LoadOrCreate(cfgPath)
					if err != nil {
						log.Errorf("Błąd reloadu: %v", err)
						continue
					}
					cfg = newCfg
					s.UpdateConfig(cfg)
					log.Infof("Konfiguracja przeładowana")

				case <-mAbout.ClickedCh:
					log.Infof("PCM2WWW Sync %s | %s", ver, runtime.Version())

				case <-mQuit.ClickedCh:
					// łagodne zamykanie
					cancel()
					s.Stop()
					systray.Quit()
					return
				}
			}
		}()
	}, func() {
		// onExit — daj chwilę loggerowi na flush (jeśli potrzebuje)
		time.Sleep(50 * time.Millisecond)
	})
}

func mustAppDataDir(name string) string {
	base, err := os.UserConfigDir()
	if err != nil {
		panic(err)
	}
	p := filepath.Join(base, name)
	_ = os.MkdirAll(p, 0o755)
	return p
}

// przenośne otwieranie plików/katalogów w domyślnej aplikacji
func openInExplorer(path string) {
	switch runtime.GOOS {
	case "windows":
		// "start" musi być uruchomiony przez cmd /C, z pustym tytułem okna ""
		_ = exec.Command("cmd", "/C", "start", "", path).Start()
	case "darwin":
		_ = exec.Command("open", path).Start()
	default:
		_ = exec.Command("xdg-open", path).Start()
	}
}
