package db

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	gmysql "gorm.io/driver/mysql"
	gpostgres "gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type OpenConfig struct {
	// sqlite | postgres | mysql
	Driver string
	// dla postgres/mysql
	DSN string
	// dla sqlite (opcjonalna ścieżka)
	Path string
}

type Handle struct {
	DB     *gorm.DB
	Path   string
	Driver string
}

func OpenAt(dir string) (*Handle, error) {
	return OpenWithConfig(dir, OpenConfig{
		Driver: "sqlite",
		Path:   filepath.Join(dir, "pcm2www.db"),
	})
}

func OpenWithConfig(appDir string, cfg OpenConfig) (*Handle, error) {
	driver := strings.ToLower(strings.TrimSpace(cfg.Driver))
	if driver == "" {
		driver = "sqlite"
	}

	switch driver {
	case "sqlite":
		dbPath, err := resolveSQLitePath(appDir, cfg.Path)
		if err != nil {
			return nil, err
		}

		gdb, err := gorm.Open(sqlite.Open(dbPath+"?_busy_timeout=5000&_journal_mode=WAL"), &gorm.Config{
			// Logger: logger.Default.LogMode(logger.Info), // włącz jeśli chcesz verbose SQL
		})
		if err != nil {
			return nil, err
		}
		if err := tunePool(gdb, driver); err != nil {
			return nil, err
		}
		return &Handle{DB: gdb, Path: dbPath, Driver: "sqlite"}, nil

	case "postgres", "postgresql":
		dsn := strings.TrimSpace(cfg.DSN)
		if dsn == "" {
			return nil, fmt.Errorf("DB dsn is empty for driver=%s", driver)
		}
		gdb, err := gorm.Open(gpostgres.Open(dsn), &gorm.Config{})
		if err != nil {
			return nil, err
		}
		if err := tunePool(gdb, "postgres"); err != nil {
			return nil, err
		}
		return &Handle{DB: gdb, Path: "postgres (dsn configured)", Driver: "postgres"}, nil

	case "mysql":
		dsn := strings.TrimSpace(cfg.DSN)
		if dsn == "" {
			return nil, fmt.Errorf("DB dsn is empty for driver=%s", driver)
		}
		gdb, err := gorm.Open(gmysql.Open(dsn), &gorm.Config{})
		if err != nil {
			return nil, err
		}
		if err := tunePool(gdb, "mysql"); err != nil {
			return nil, err
		}
		return &Handle{DB: gdb, Path: "mysql (dsn configured)", Driver: "mysql"}, nil

	default:
		return nil, fmt.Errorf("unsupported DB driver: %s (allowed: sqlite|postgres|mysql)", cfg.Driver)
	}
}

func tunePool(gdb *gorm.DB, driver string) error {
	sqlDB, err := gdb.DB()
	if err != nil {
		return err
	}

	switch driver {
	case "sqlite":
		sqlDB.SetMaxOpenConns(1)
		sqlDB.SetMaxIdleConns(1)
		sqlDB.SetConnMaxLifetime(0)
	default:
		sqlDB.SetMaxOpenConns(10)
		sqlDB.SetMaxIdleConns(5)
		sqlDB.SetConnMaxLifetime(1 * time.Hour)
	}
	return nil
}

func resolveSQLitePath(appDir, p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		p = filepath.Join(appDir, "pcm2www.db")
	}

	p = expandHome(p)
	if !filepath.IsAbs(p) {
		p = filepath.Join(appDir, p)
	}

	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	return p, nil
}

func expandHome(p string) string {
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~/"))
		}
	}
	return p
}
