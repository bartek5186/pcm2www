package logview

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

var fieldLabels = map[string]string{
	"archived_path": "archiwum",
	"code":          "kod HTTP",
	"db":            "plik bazy",
	"dir":           "katalog",
	"driver":        "silnik bazy",
	"file":          "plik",
	"import_id":     "import",
	"integration":   "integracja",
	"kind":          "operacja",
	"page":          "strona",
	"reason":        "powód",
	"removed":       "usunięte",
	"retry_in":      "ponowienie za",
	"shop":          "sklep",
	"status":        "status",
	"task_id":       "zadanie",
	"tasks":         "zadania",
	"upserts":       "zmienione",
}

var messageTranslations = map[string]string{
	"DB ready":                               "Baza danych gotowa",
	"start":                                  "Integracja uruchomiona",
	"stop":                                   "Integracja zatrzymana",
	"syncer started":                         "Synchronizacja uruchomiona",
	"syncer stopped":                         "Synchronizacja zatrzymana",
	"syncer configuration reloaded":          "Konfiguracja synchronizacji przeładowana",
	"integration stopped with error":         "Integracja zatrzymana z błędem",
	"recovered interrupted Woo tasks":        "Przywrócono przerwane zadania WooCommerce",
	"Woo cache primed (products)":            "Pobrano cache produktów WooCommerce",
	"cache sweeper disabled (interval <= 0)": "Automatyczne odświeżanie cache jest wyłączone",
}

var errorTranslations = map[string]string{
	`brak integracji "importer" w configu`: "Brakuje ustawień importera w konfiguracji",
	`syncer: initialize integration "woocommerce": woocommerce: configure real consumer_key and consumer_secret`: "Nie można uruchomić WooCommerce: uzupełnij prawidłowy Consumer key i Consumer secret",
}

// Reader returns a bounded, human-readable snapshot of the newest log entries.
// It skips disk reads when the file size and modification time did not change.
type Reader struct {
	path        string
	maxBytes    int64
	initialized bool
	lastSize    int64
	lastModTime time.Time
}

func NewReader(path string, maxBytes int64) *Reader {
	return &Reader{path: path, maxBytes: maxBytes}
}

func (r *Reader) Read() (text string, changed bool, err error) {
	file, err := os.Open(r.path)
	if err != nil {
		return "", false, fmt.Errorf("otwarcie pliku logu: %w", err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return "", false, fmt.Errorf("odczyt informacji o pliku logu: %w", err)
	}
	if r.initialized && info.Size() == r.lastSize && info.ModTime().Equal(r.lastModTime) {
		return "", false, nil
	}

	raw, err := readTail(file, info.Size(), r.maxBytes)
	if err != nil {
		return "", false, fmt.Errorf("odczyt pliku logu: %w", err)
	}
	r.initialized = true
	r.lastSize = info.Size()
	r.lastModTime = info.ModTime()
	return format(raw), true, nil
}

func readTail(file *os.File, size, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 || size <= 0 {
		return nil, nil
	}
	start := size - maxBytes
	if start < 0 {
		start = 0
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes))
	if err != nil {
		return nil, err
	}
	if start > 0 {
		if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
			data = data[newline+1:]
		}
	}
	return data, nil
}

func format(raw []byte) string {
	var output bytes.Buffer
	console := zerolog.ConsoleWriter{
		Out:          &output,
		NoColor:      true,
		TimeFormat:   "02.01.2006 15:04:05",
		TimeLocation: time.Local,
		PartsOrder: []string{
			zerolog.TimestampFieldName,
			zerolog.LevelFieldName,
			zerolog.MessageFieldName,
		},
		FormatLevel:         formatLevel,
		FormatMessage:       formatMessage,
		FormatFieldName:     formatFieldName,
		FormatFieldValue:    formatValue,
		FormatErrFieldName:  func(any) string { return "— " },
		FormatErrFieldValue: formatError,
	}

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSuffix(scanner.Bytes(), []byte{'\r'})
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		// Older app versions wrote these periodically. Keep them out of the
		// viewer as well; the live indicator now reports the current state.
		var entry struct {
			Level   string `json:"level"`
			Message string `json:"message"`
		}
		if json.Unmarshal(line, &entry) == nil && entry.Level == "debug" && entry.Message == "syncer heartbeat" {
			continue
		}
		if _, err := console.Write(line); err != nil {
			output.WriteString(strings.ToValidUTF8(string(line), "�"))
			output.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		output.WriteString("[błąd formatowania logu: " + err.Error() + "]\n")
	}

	text := strings.TrimRight(output.String(), "\r\n")
	return strings.ReplaceAll(text, "\n", "\r\n")
}

func formatLevel(value any) string {
	var label string
	switch strings.ToLower(fmt.Sprint(value)) {
	case "trace":
		label = "ŚLAD"
	case "debug":
		label = "DEBUG"
	case "info":
		label = "INFO"
	case "warn", "warning":
		label = "UWAGA"
	case "error":
		label = "BŁĄD"
	case "fatal", "panic":
		label = "KRYTYCZNY"
	default:
		label = "LOG"
	}
	return fmt.Sprintf("%-11s", "["+label+"]")
}

func formatFieldName(value any) string {
	name := fmt.Sprint(value)
	if friendly, ok := fieldLabels[name]; ok {
		name = friendly
	}
	return "• " + name + ": "
}

func formatValue(value any) string {
	text := fmt.Sprint(value)
	if unquoted, err := strconv.Unquote(text); err == nil {
		return unquoted
	}
	return text
}

func formatMessage(value any) string {
	message := formatValue(value)
	if friendly, ok := messageTranslations[message]; ok {
		return friendly
	}
	return message
}

func formatError(value any) string {
	errorText := formatValue(value)
	if friendly, ok := errorTranslations[errorText]; ok {
		return friendly
	}
	return errorText
}
