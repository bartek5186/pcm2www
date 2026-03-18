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
	"unsafe"

	conf "github.com/bartek5186/pcm2www/internal/config"
	"github.com/bartek5186/pcm2www/internal/db"
	logs "github.com/bartek5186/pcm2www/internal/logs"
	syncer "github.com/bartek5186/pcm2www/internal/syncer"
	"github.com/getlantern/systray"
)

//go:embed assets/favicon.ico
var iconData []byte

var ver = "1.0.0"
var buildDate = "unknown" // ustawiane przez: -ldflags "-X main.buildDate=2026-03-18"

func main() {
	defer func() {
		if r := recover(); r != nil {
			messageBox("Procyon Syncer — błąd startu", fmt.Sprintf("Nieoczekiwany błąd:\n%v", r))
		}
	}()

	appDir, err := os.UserConfigDir()
	if err != nil {
		messageBox("Procyon Syncer — błąd startu", fmt.Sprintf("Nie można ustalić katalogu AppData:\n%v", err))
		return
	}
	appDir = filepath.Join(appDir, "pcm2www")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		messageBox("Procyon Syncer — błąd startu", fmt.Sprintf("Nie można utworzyć katalogu:\n%s\n%v", appDir, err))
		return
	}

	logFile, err := os.OpenFile(filepath.Join(appDir, "app.log"), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		messageBox("Procyon Syncer — błąd startu", fmt.Sprintf("Nie można otworzyć pliku log:\n%v", err))
		return
	}
	logFile.Close()

	log := logs.New(filepath.Join(appDir, "app.log"), false)

	cfgPath := filepath.Join(appDir, "config.json")
	cfg, firstRun, err := conf.LoadOrCreate(cfgPath)
	if err != nil {
		messageBox("Procyon Syncer — błąd startu", fmt.Sprintf("Błąd konfiguracji:\n%v", err))
		return
	}
	if firstRun {
		log.Info().Msgf("Utworzono domyślną konfigurację: %s", cfgPath)
	}

	dbh, err := db.OpenWithConfig(appDir, db.OpenConfig{
		Driver: cfg.Database.Driver,
		DSN:    cfg.Database.DSN,
		Path:   cfg.Database.Path,
	})
	if err != nil {
		messageBox("Procyon Syncer — błąd startu", fmt.Sprintf("Błąd otwarcia bazy danych:\n%v", err))
		return
	}
	if err := dbh.Migrate(); err != nil {
		messageBox("Procyon Syncer — błąd startu", fmt.Sprintf("Błąd migracji bazy danych:\n%v", err))
		return
	}
	log.Info().Str("driver", dbh.Driver).Str("db", dbh.Path).Msg("DB ready")

	log.Info().Msg("Aplikacja uruchomiona")
	sqlDB, _ := dbh.DB.DB()
	defer sqlDB.Close()

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
		systray.SetTooltip(fmt.Sprintf("Procyon Syncer %s", ver))

		mStart := systray.AddMenuItem("Start synchronizacji", "Uruchom harmonogram")
		mStop := systray.AddMenuItem("Stop synchronizacji", "Zatrzymaj harmonogram")
		mStop.Disable()

		systray.AddSeparator()
		mOpenLogs := systray.AddMenuItem("Otwórz logi", "Pokaż plik log")
		mOpenCfg := systray.AddMenuItem("Ustawienia (config.json)", "Otwórz plik konfiguracyjny")
		mReload := systray.AddMenuItem("Przeładuj konfigurację", "Wczytaj ponownie config.json")
		systray.AddSeparator()
		mAbout := systray.AddMenuItem(fmt.Sprintf("Procyon Syncer %s", ver), "O programie")
		mQuit := systray.AddMenuItem("Wyjście", "Zamknij aplikację")

		if cfg.AutoStart {
			if err := s.Start(ctx); err == nil {
				mStart.Disable()
				mStop.Enable()
				systray.SetTooltip(fmt.Sprintf("Procyon Syncer %s — działa", ver))
			} else {
				log.Error().Msgf("AutoStart nieudany: %v", err)
				systray.SetTooltip(fmt.Sprintf("Procyon Syncer %s — błąd startu", ver))
			}
		}

		go func() {
			for {
				select {
				case <-mStart.ClickedCh:
					if err := s.Start(ctx); err != nil {
						log.Error().Msgf("Start error: %v", err)
						systray.SetTooltip(fmt.Sprintf("Procyon Syncer %s — błąd startu", ver))
						continue
					}
					mStart.Disable()
					mStop.Enable()
					systray.SetTooltip(fmt.Sprintf("Procyon Syncer %s — działa", ver))

				case <-mStop.ClickedCh:
					s.Stop()
					mStop.Disable()
					mStart.Enable()
					systray.SetTooltip(fmt.Sprintf("Procyon Syncer %s — zatrzymane", ver))

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
					msg := fmt.Sprintf(
						"Procyon Syncer %s\nBuild: %s\n\nInterfejs pcm2www dla PC-Market 7.\nSynchronizacja stanów magazynowych, cen\ni dostępności produktów z WooCommerce.\n\nLogi: %s\n\nAutor: Bartek5186\nhttps://github.com/bartek5186",
						ver, buildDate, appDir,
					)
					messageBoxWithIcon("O programie", msg)

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


var (
	modUser32      = syscall.NewLazyDLL("user32.dll")
	procMessageBox = modUser32.NewProc("MessageBoxW")
)

func messageBox(title, text string) {
	t, _ := syscall.UTF16PtrFromString(title)
	m, _ := syscall.UTF16PtrFromString(text)
	_, _, _ = procMessageBox.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), 0x40)
}

func messageBoxWithIcon(title, text string) {
	messageBox(title, text)
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
