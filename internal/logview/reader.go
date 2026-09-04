package logview

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

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
		TimeFormat:   "2006-01-02 15:04:05",
		TimeLocation: time.Local,
	}

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSuffix(scanner.Bytes(), []byte{'\r'})
		if len(bytes.TrimSpace(line)) == 0 {
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
