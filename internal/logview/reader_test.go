package logview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReaderFormatsNewEntriesAndSkipsUnchangedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	raw := `{"level":"info","time":"2026-09-04T10:00:00+02:00","caller":"main.go:10","items":3,"message":"Synchronizacja działa"}` + "\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	reader := NewReader(path, 1024)
	text, changed, err := reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("first read should report a change")
	}
	for _, expected := range []string{"INF", "Synchronizacja działa", "main.go:10", "items=3"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("formatted log missing %q: %q", expected, text)
		}
	}

	if text, changed, err := reader.Read(); err != nil {
		t.Fatal(err)
	} else if changed || text != "" {
		t.Fatalf("unchanged file should not be returned again: changed=%v text=%q", changed, text)
	}
}

func TestReaderDetectsAppendAndTruncate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader := NewReader(path, 1024)
	if _, _, err := reader.Read(); err != nil {
		t.Fatal(err)
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("second\n"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	text, changed, err := reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !changed || !strings.Contains(text, "first") || !strings.Contains(text, "second") {
		t.Fatalf("append was not detected: changed=%v text=%q", changed, text)
	}

	if err := os.WriteFile(path, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	text, changed, err = reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !changed || strings.Contains(text, "first") || !strings.Contains(text, "new") {
		t.Fatalf("truncation was not detected: changed=%v text=%q", changed, text)
	}
}

func TestReadTailStartsAtCompleteLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	raw := "very-old-line\nrecent-one\nrecent-two\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	data, err := readTail(file, int64(len(raw)), int64(len("xrecent-one\nrecent-two\n")))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "recent-one\nrecent-two\n" {
		t.Fatalf("unexpected tail: %q", data)
	}
}
