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
	"github.com/bartek5186/pcm2www/internal/singleinstance"
	syncer "github.com/bartek5186/pcm2www/internal/syncer"
	"gorm.io/gorm"
)

var ver = "1.0.0"

func main() {
	appDir := mustAppDataDir("pcm2www")
	instanceLock, err := singleinstance.Acquire(filepath.Join(appDir, "pcm2www.lock"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	defer instanceLock.Release()
	log := logs.New(filepath.Join(appDir, "app.log"), true)

	cfgPath := filepath.Join(appDir, "config.json")
	cfg, firstRun, err := conf.LoadOrCreate(cfgPath)
	if err != nil {
		panic(err)
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
		log.Fatal().Err(err).Msg("DB open error")
	}
	if err := dbh.Migrate(); err != nil {
		log.Fatal().Err(err).Msg("DB migrate error")
	}
	log.Info().Str("driver", dbh.Driver).Str("db", dbh.Path).Msg("DB ready")
	sqlDB, _ := dbh.DB.DB()
	defer sqlDB.Close()

	log.Info().Msg("Aplikacja (CLI) uruchomiona")

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
	fmt.Println("Komendy: start | stop | reload | status | retry-errors | paths | resetdb | quit")
	reader := bufio.NewReader(os.Stdin)

	for {
		fmt.Print("> ")
		line, readErr := reader.ReadString('\n')
		if readErr != nil && len(line) == 0 {
			cancel()
			s.Stop()
			return
		}
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
			if err := s.UpdateConfig(newCfg); err != nil {
				log.Error().Err(err).Msg("Błąd zastosowania konfiguracji")
				fmt.Println("Błąd reloadu:", err)
				continue
			}
			cfg = newCfg
			log.Info().Msg("Konfiguracja przeładowana")
			fmt.Println("Konfiguracja przeładowana")
		case "status":
			if s.IsRunning() {
				fmt.Println("Status: DZIAŁA")
			} else {
				fmt.Println("Status: ZATRZYMANY")
			}
			for _, status := range s.IntegrationStatuses() {
				fmt.Printf("  %s: %s", status.Name, status.State)
				if status.LastError != "" {
					fmt.Printf(" (%s)", status.LastError)
				}
				fmt.Println()
			}
			printQueueDiagnostics(dbh.DB)
		case "retry-errors":
			res := dbh.DB.Model(&db.WooTask{}).Where("status = ?", "error").Updates(map[string]any{
				"status": "pending", "attempts": 0, "last_error": "", "next_attempt_at": nil, "started_at": nil, "finished_at": nil,
			})
			if res.Error != nil {
				fmt.Println("Błąd:", res.Error)
				continue
			}
			fmt.Printf("Ponowiono %d tasków.\n", res.RowsAffected)
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

			s.Stop()
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
			fmt.Println("Nieznana komenda. Użyj: start | stop | reload | status | retry-errors | paths | resetdb | quit")
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
	}

	return gdb.Transaction(func(tx *gorm.DB) error {
		for _, table := range tables {
			if err := tx.Exec(fmt.Sprintf("DELETE FROM %s;", table)).Error; err != nil {
				return fmt.Errorf("błąd czyszczenia tabeli %s: %w", table, err)
			}
		}
		return nil
	})
}

func printQueueDiagnostics(gdb *gorm.DB) {
	type row struct {
		Status string
		Count  int64
	}
	var rows []row
	if err := gdb.Model(&db.WooTask{}).Select("status, COUNT(*) AS count").Group("status").Find(&rows).Error; err != nil {
		fmt.Println("  kolejka: błąd odczytu:", err)
		return
	}
	if len(rows) == 0 {
		fmt.Println("  kolejka: pusta")
		return
	}
	fmt.Print("  kolejka:")
	for _, row := range rows {
		fmt.Printf(" %s=%d", row.Status, row.Count)
	}
	fmt.Println()
}
