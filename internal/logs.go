package logs

import (
	"os"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func New(logFilePath string) zerolog.Logger {
	// Utwórz plik logów (append + tworzenie jeśli brak)
	file, err := os.OpenFile(logFilePath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		log.Fatal().Err(err).Msg("Nie można otworzyć pliku log")
	}

	// Ustawienia formatu (czas, poziom itd.)
	logger := zerolog.New(file).With().
		Timestamp().
		Caller().
		Logger()

	// Ustaw globalny logger
	log.Logger = logger
	zerolog.TimeFieldFormat = time.RFC3339

	return logger
}
