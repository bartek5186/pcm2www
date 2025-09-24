//go:build windows && !dev

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

	conf "github.com/bartek5186/pcm2www/internal/config"
	"github.com/bartek5186/pcm2www/internal/db"
	logs "github.com/bartek5186/pcm2www/internal/logs"
	syncer "github.com/bartek5186/pcm2www/internal/syncer"
	"github.com/getlantern/systray"
)

//go:embed assets/favicon.ico
var iconData []byte

var ver = "1.0.0"

func main() {
	appDir := mustAppDataDir("pcm2www")
	log := logs.New(filepath.Join(appDir, "app.log"), false)

	dbh, err := db.OpenAt(appDir)
	if err != nil {
		log.Fatal().Err(err).Msg("DB open error")
	}
	if err := dbh.Migrate(); err != nil {
		log.Fatal().Err(err).Msg("DB migrate error")
	}
	log.Info().Str("db", dbh.Path).Msg("DB ready")

	log.Info().Msg("Aplikacja uruchomiona")

	cfgPath := filepath.Join(appDir, "config.json")
	cfg, firstRun, err := conf.LoadOrCreate(cfgPath)
	if err != nil {
		panic(err)
	}
	if firstRun {
		log.Info().Msgf("Utworzono domyślną konfigurację: %s", cfgPath)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	s := syncer.New(log, cfg, dbh.DB)

	go func() {
		<-ctx.Done()
		s.Stop()
		systray.Quit()
	}()

	systray.Run(func() {
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

		if cfg.AutoStart {
			if err := s.Start(ctx); err == nil {
				mStart.Disable()
				mStop.Enable()
				systray.SetTooltip(fmt.Sprintf("PCM2WWW Sync %s — działa", ver))
			} else {
				log.Error().Msgf("AutoStart nieudany: %v", err)
				systray.SetTooltip(fmt.Sprintf("PCM2WWW Sync %s — błąd startu", ver))
			}
		}

		go func() {
			for {
				select {
				case <-mStart.ClickedCh:
					if err := s.Start(ctx); err != nil {
						log.Error().Msgf("Start error: %v", err)
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
						log.Error().Msgf("Błąd reloadu: %v", err)
						continue
					}
					cfg = newCfg
					s.UpdateConfig(cfg)
					log.Info().Msg("Konfiguracja przeładowana")

				case <-mAbout.ClickedCh:
					log.Info().Msgf("PCM2WWW Sync %s | %s", ver, runtime.Version())

				case <-mQuit.ClickedCh:
					cancel()
					s.Stop()
					systray.Quit()
					return
				}
			}
		}()
	}, func() {
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

func openInExplorer(path string) {
	switch runtime.GOOS {
	case "windows":
		_ = exec.Command("cmd", "/C", "start", "", path).Start()
	case "darwin":
		_ = exec.Command("open", path).Start()
	default:
		_ = exec.Command("xdg-open", path).Start()
	}
}
