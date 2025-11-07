//go:build !windows || dev

package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	conf "github.com/bartek5186/pcm2www/internal/config"
	"github.com/bartek5186/pcm2www/internal/db"
	logs "github.com/bartek5186/pcm2www/internal/logs"
	syncer "github.com/bartek5186/pcm2www/internal/syncer"
	"gorm.io/gorm"
)

var ver = "1.0.0"

func main() {
	appDir := mustAppDataDir("pcm2www")
	log := logs.New(filepath.Join(appDir, "app.log"), true)

	dbh, err := db.OpenAt(appDir)
	if err != nil {
		log.Fatal().Err(err).Msg("DB open error")
	}
	if err := dbh.Migrate(); err != nil {
		log.Fatal().Err(err).Msg("DB migrate error")
	}
	log.Info().Str("db", dbh.Path).Msg("DB ready")
	sqlDB, _ := dbh.DB.DB()
	defer sqlDB.Close()

	log.Info().Msg("Aplikacja (CLI) uruchomiona")

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

	// AutoStart tak jak w GUI
	if cfg.AutoStart {
		if err := s.Start(ctx); err != nil {
			log.Error().Msgf("AutoStart nieudany: %v", err)
		} else {
			log.Info().Msgf("PCM2WWW Sync %s — działa", ver)
		}
	}

	// Prosta pętla poleceń w terminalu
	fmt.Println("PCM2WWW CLI", ver)
	fmt.Println("Komendy: start | stop | reload | status | paths | resetdb! | quit")
	reader := bufio.NewReader(os.Stdin)

	for {
		fmt.Print("> ")
		line, _ := reader.ReadString('\n')
		cmd := strings.TrimSpace(strings.ToLower(line))

		switch cmd {
		case "start":
			if err := s.Start(ctx); err != nil {
				log.Error().Msgf("Start error: %v", err)
				fmt.Println("Błąd startu:", err)
				continue
			}
			fmt.Println("Start OK")
		case "stop":
			s.Stop()
			fmt.Println("Zatrzymano")
		case "reload":
			newCfg, _, err := conf.LoadOrCreate(cfgPath)
			if err != nil {
				log.Error().Msgf("Błąd reloadu: %v", err)
				fmt.Println("Błąd reloadu:", err)
				continue
			}
			cfg = newCfg
			s.UpdateConfig(cfg)
			log.Info().Msg("Konfiguracja przeładowana")
			fmt.Println("Konfiguracja przeładowana")
		case "status":
			if r, ok := any(s).(interface{ IsRunning() bool }); ok {
				if r.IsRunning() {
					fmt.Println("Status: DZIAŁA")
				} else {
					fmt.Println("Status: ZATRZYMANY")
				}
			} else {
				fmt.Println("Status: (syncer nie wystawia IsRunning)")
			}
		case "paths":
			fmt.Println("Logi:", filepath.Join(appDir, "app.log"))
			fmt.Println("Config:", cfgPath)
		case "quit", "exit":
			cancel()
			s.Stop()
			time.Sleep(50 * time.Millisecond)
			return
		case "resetdb":
			fmt.Print("Na pewno chcesz wyczyścić bazę? (tak/nie): ")
			confirm, _ := reader.ReadString('\n')
			confirm = strings.TrimSpace(strings.ToLower(confirm))
			if confirm != "tak" {
				fmt.Println("Anulowano.")
				continue
			}

			log.Warn().Msg("Czyszczenie bazy...")
			if err := resetDB(dbh.DB); err != nil {
				log.Error().Err(err).Msg("Błąd czyszczenia bazy")
				fmt.Println("Błąd:", err)
				continue
			}
			fmt.Println("Baza wyczyszczona.")
			return
		case "":
			// enter – ignoruj
		default:
			fmt.Println("Nieznana komenda. Użyj: start | stop | reload | status | paths | resetdb! | quit")
		}
	}
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

// resetDB usuwa wszystkie dane z głównych tabel testowych.
func resetDB(gdb *gorm.DB) error {
	tables := []string{
		"import_files",
		"st_products",
		"st_stocks",
		"woo_product_caches",
		"woo_tasks",
		"link_issues",
		"kvs",
		"sqlite_sequence",
	}

	for _, t := range tables {
		if err := gdb.Exec(fmt.Sprintf("DELETE FROM %s;", t)).Error; err != nil {
			return fmt.Errorf("błąd czyszczenia tabeli %s: %w", t, err)
		}
	}
	return nil
}
