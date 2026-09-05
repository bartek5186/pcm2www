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
	for _, expected := range []string{"[INFO", "Synchronizacja działa", "• items: 3"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("formatted log missing %q: %q", expected, text)
		}
	}
	if strings.Contains(text, "main.go:10") {
		t.Fatalf("user-facing log should not contain source location: %q", text)
	}

	if text, changed, err := reader.Read(); err != nil {
		t.Fatal(err)
	} else if changed || text != "" {
		t.Fatalf("unchanged file should not be returned again: changed=%v text=%q", changed, text)
	}
}

func TestReaderFormatsErrorAsUserFacingMessage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	raw := `{"level":"error","time":"2026-09-04T10:00:00+02:00","caller":"main.go:192","error":"brak integracji \"importer\" w configu","message":"Błąd okna ustawień"}` + "\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	text, _, err := NewReader(path, 1024).Read()
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"[BŁĄD]", "Błąd okna ustawień", "— Brakuje ustawień importera w konfiguracji"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("friendly error log missing %q: %q", expected, text)
		}
	}
	if strings.Contains(text, "main.go:192") || strings.Contains(text, "error=") {
		t.Fatalf("friendly error log contains technical decoration: %q", text)
	}
}

func TestReaderTranslatesCommonTechnicalMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	raw := `{"level":"info","time":"2026-09-04T10:00:00+02:00","caller":"main.go:88","db":"C:\\pcm2www.db","driver":"sqlite","message":"DB ready"}` + "\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	text, _, err := NewReader(path, 1024).Read()
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Baza danych gotowa", `• plik bazy: C:\pcm2www.db`, "• silnik bazy: sqlite"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("translated log missing %q: %q", expected, text)
		}
	}
	if strings.Contains(text, "DB ready") || strings.Contains(text, "main.go:88") {
		t.Fatalf("translated log contains technical message: %q", text)
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

func TestReaderHidesLegacyHeartbeatWithoutHidingErrors(t *testing.T) {
	raw := `{"level":"debug","tick":1,"message":"syncer heartbeat"}` + "\n" +
		`{"level":"error","message":"integration stopped with error","error":"decode page 4"}` + "\n" +
		`{"level":"debug","message":"another debug message"}` + "\n"
	text := format([]byte(raw))
	if strings.Contains(text, "heartbeat") || strings.Contains(text, "tick") {
		t.Fatalf("legacy heartbeat remains visible: %q", text)
	}
	if !strings.Contains(text, "decode page 4") || !strings.Contains(text, "another debug message") {
		t.Fatalf("unrelated logs were hidden: %q", text)
	}
}
