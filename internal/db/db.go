package db

import (
	"path/filepath"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type Handle struct {
	DB   *gorm.DB
	Path string
}

func OpenAt(dir string) (*Handle, error) {
	dbPath := filepath.Join(dir, "pcm2www.db")
	gdb, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{
		// Logger: logger.Default.LogMode(logger.Info), // włącz jeśli chcesz verbose SQL
	})
	if err != nil {
		return nil, err
	}
	return &Handle{DB: gdb, Path: dbPath}, nil
}
