package logs

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func New(logFilePath string, withConsole bool) zerolog.Logger {
	logFile, err := newRotatingFileWriter(logFilePath, 10<<20, 5)
	if err != nil {
		log.Fatal().Err(err).Msg("Nie można otworzyć pliku log")
	}

	// Format czasu
	zerolog.TimeFieldFormat = time.RFC3339

	var writer io.Writer = logFile

	if withConsole {
		consoleWriter := zerolog.ConsoleWriter{
			Out:        os.Stdout,
			TimeFormat: time.RFC3339,
		}
		writer = zerolog.MultiLevelWriter(logFile, consoleWriter)
	}

	// Logger z timestampem i info o miejscu wywołania
	logger := zerolog.New(writer).With().
		Timestamp().
		Caller().
		Logger()

	// Ustaw globalny logger
	log.Logger = logger

	return logger
}

type rotatingFileWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	backups  int
	file     *os.File
}

func newRotatingFileWriter(path string, maxBytes int64, backups int) (*rotatingFileWriter, error) {
	w := &rotatingFileWriter{path: path, maxBytes: maxBytes, backups: backups}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *rotatingFileWriter) open() error {
	file, err := os.OpenFile(w.path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return err
	}
	w.file = file
	return nil
}

func (w *rotatingFileWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if info, err := w.file.Stat(); err != nil {
		return 0, err
	} else if info.Size()+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	return w.file.Write(p)
}

func (w *rotatingFileWriter) rotate() error {
	if err := w.file.Close(); err != nil {
		return err
	}
	for n := w.backups - 1; n >= 1; n-- {
		oldPath := fmt.Sprintf("%s.%d", w.path, n)
		newPath := fmt.Sprintf("%s.%d", w.path, n+1)
		if err := os.Rename(oldPath, newPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if w.backups > 0 {
		if err := os.Rename(w.path, w.path+".1"); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return w.open()
}
