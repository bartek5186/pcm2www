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
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/bartek5186/pcm2www/internal/appsettings"
	conf "github.com/bartek5186/pcm2www/internal/config"
	"github.com/bartek5186/pcm2www/internal/db"
	logs "github.com/bartek5186/pcm2www/internal/logs"
	"github.com/bartek5186/pcm2www/internal/singleinstance"
	syncer "github.com/bartek5186/pcm2www/internal/syncer"
	"github.com/getlantern/systray"
	"github.com/lxn/walk"
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
	instanceLock, err := singleinstance.Acquire(filepath.Join(appDir, "pcm2www.lock"))
	if err != nil {
		messageBox("Procyon Syncer — już uruchomiony", err.Error())
		return
	}
	defer instanceLock.Release()

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
	var configState atomic.Pointer[conf.Config]
	configState.Store(cfg)
	var configChangeMu sync.Mutex
	uiHost, err := startWindowsUI(ctx, log)
	if err != nil {
		messageBox("Procyon Syncer — interfejs", err.Error())
		return
	}

	go func() {
		<-ctx.Done()
		s.Stop()
		systray.Quit()
	}()

	systray.Run(func() {
		if len(iconData) > 0 {
			systray.SetIcon(iconData)
		}
		mStatus := systray.AddMenuItem("Synchronizacja zatrzymana", "Otwórz logi i szczegóły stanu")
		systray.AddSeparator()
		mStart := systray.AddMenuItem("Start synchronizacji", "Uruchom harmonogram")
		mStop := systray.AddMenuItem("Stop synchronizacji", "Zatrzymaj harmonogram")

		systray.AddSeparator()
		mOpenLogs := systray.AddMenuItem("Otwórz logi", "Pokaż logi na żywo")
		mProblems := systray.AddMenuItem("Analityka", "Aktualne braki EAN, błędy i linki do produktów")
		mSettings := systray.AddMenuItem("Ustawienia…", "Otwórz okno ustawień")
		mReload := systray.AddMenuItem("Przeładuj konfigurację", "Wczytaj ponownie config.json")
		systray.AddSeparator()
		mAbout := systray.AddMenuItem(fmt.Sprintf("Procyon Syncer %s", ver), "O programie")
		mQuit := systray.AddMenuItem("Wyjście", "Zamknij aplikację")

		var logWindow, settingsWindow, problemsWindow *walk.Dialog // UI thread only
		openLogs := func() {
			if raiseDialog(logWindow) {
				return
			}
			var err error
			logWindow, err = showLogWindow(filepath.Join(appDir, "app.log"), s)
			if err != nil {
				log.Error().Err(err).Msg("Nie można otworzyć okna logów")
				messageBox("Procyon Syncer — logi", err.Error())
			}
		}
		var previousStatus syncer.StatusSnapshot
		var previousMenuColor uint32
		refreshStatus := func() {
			status := s.Status()
			menuColor := statusMenuBackground()
			if status == previousStatus && menuColor == previousMenuColor {
				return
			}
			previousStatus = status
			previousMenuColor = menuColor
			_, statusIcon := statusAppearance(status.State)
			mStatus.SetTitle(status.Text)
			mStatus.SetIcon(statusIcon)
			systray.SetTooltip(fmt.Sprintf("Procyon Syncer %s — %s", ver, status.Text))
			if status.Active {
				mStart.Disable()
				mStop.Enable()
			} else {
				mStop.Disable()
				mStart.Enable()
			}
		}
		refreshStatus()
		// Status polling stays independent of configuration reloads and UI work.
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if ctx.Err() != nil {
						return
					}
					refreshStatus()
				}
			}
		}()

		if cfg.AutoStart {
			if err := s.Start(ctx); err != nil {
				log.Error().Msgf("AutoStart nieudany: %v", err)
			}
		}

		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-mStart.ClickedCh:
					go func() {
						if err := s.Start(ctx); err != nil {
							log.Error().Msgf("Start error: %v", err)
						}
					}()

				case <-mStop.ClickedCh:
					go s.Stop()

				case <-mStatus.ClickedCh:
					dispatchUI(uiHost, log, openLogs)

				case <-mOpenLogs.ClickedCh:
					dispatchUI(uiHost, log, openLogs)

				case <-mProblems.ClickedCh:
					dispatchUI(uiHost, log, func() {
						if raiseDialog(problemsWindow) {
							return
						}
						var wooConfig struct {
							BaseURL string `json:"base_url"`
						}
						if err := configState.Load().UnmarshalIntegration("woocommerce", &wooConfig); err != nil {
							log.Warn().Err(err).Msg("Raport problemów: brak adresu sklepu do linków")
						}
						var err error
						problemsWindow, err = showProblemsWindow(ctx, dbh.DB, wooConfig.BaseURL, s)
						if err != nil {
							log.Error().Err(err).Msg("Nie można otworzyć raportu problemów")
							messageBox("Procyon Syncer — problemy", err.Error())
						}
					})

				case <-mSettings.ClickedCh:
					dispatchUI(uiHost, log, func() {
						if raiseDialog(settingsWindow) {
							return
						}
						base := configState.Load()
						var err error
						settingsWindow, err = showSettingsWindow(base, cfgPath, func(candidate *conf.Config) error {
							configChangeMu.Lock()
							defer configChangeMu.Unlock()
							if configState.Load() != base {
								return fmt.Errorf("konfiguracja została przeładowana; otwórz ustawienia ponownie")
							}
							if err := appsettings.SaveAndApply(cfgPath, base, candidate, s); err != nil {
								log.Error().Err(err).Msg("Nie zapisano ustawień")
								return err
							}
							configState.Store(candidate)
							return nil
						}, func(*conf.Config) { log.Info().Msg("Ustawienia zapisane i zastosowane") })
						if err != nil {
							log.Error().Err(err).Msg("Błąd okna ustawień")
							messageBox("Procyon Syncer — ustawienia", err.Error())
						}
					})

				case <-mReload.ClickedCh:
					go func() {
						configChangeMu.Lock()
						defer configChangeMu.Unlock()
						newCfg, _, err := conf.LoadOrCreate(cfgPath)
						if err == nil {
							err = s.UpdateConfig(newCfg)
						}
						if err != nil {
							log.Error().Err(err).Msg("Błąd przeładowania konfiguracji")
							return
						}
						configState.Store(newCfg)
						log.Info().Msg("Konfiguracja przeładowana")
					}()

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

func openInExplorer(path string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("cmd", "/C", "start", "", path).Start()
	case "darwin":
		return exec.Command("open", path).Start()
	default:
		return exec.Command("xdg-open", path).Start()
	}
}
