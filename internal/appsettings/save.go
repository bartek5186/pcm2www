package appsettings

import (
	"fmt"
	conf "github.com/bartek5186/pcm2www/internal/config"
)

type Runtime interface {
	IsRunning() bool
	ValidateConfig(*conf.Config) error
	UpdateConfig(*conf.Config) error
}

// SaveAndApply permits drafts while stopped. A live replacement must be
// runnable before the saved configuration or active integrations are changed.
func SaveAndApply(path string, previous, candidate *conf.Config, runtime Runtime) error {
	if err := candidate.Validate(); err != nil {
		return err
	}
	if runtime.IsRunning() {
		if err := runtime.ValidateConfig(candidate); err != nil {
			return err
		}
	}
	if err := conf.Save(path, candidate); err != nil {
		return fmt.Errorf("zapis config.json: %w", err)
	}
	if err := runtime.UpdateConfig(candidate); err != nil {
		if rollbackErr := conf.Save(path, previous); rollbackErr != nil {
			return fmt.Errorf("zastosowanie: %v; przywrócenie pliku: %w", err, rollbackErr)
		}
		return fmt.Errorf("zastosowanie konfiguracji: %w", err)
	}
	return nil
}
