package logs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRotatingFileWriterKeepsBoundedBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	w, err := newRotatingFileWriter(path, 8, 2)
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := w.Write([]byte("12345678")); err != nil {
			t.Fatal(err)
		}
	}
	for _, suffix := range []string{"", ".1", ".2"} {
		if _, err := os.Stat(path + suffix); err != nil {
			t.Fatalf("expected rotated log %s: %v", path+suffix, err)
		}
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("rotation retained more than two backups: %v", err)
	}
}
