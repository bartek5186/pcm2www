package logs

import (
	"io"
	"os"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func New(logFilePath string, withConsole bool) zerolog.Logger {
	// Utwórz plik logów (append + tworzenie jeśli brak)
	logFile, err := os.OpenFile(logFilePath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		log.Fatal().Err(err).Msg("Nie można otworzyć pliku log")
	}

	// Format czasu
	zerolog.TimeFieldFormat = time.RFC3339

	var writer io.Writer = logFile // <- tu jasno wskazujemy io.Writer

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
