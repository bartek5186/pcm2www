//go:build windows && !dev

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows/registry"
)

const (
	windowsRunKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`
	windowsRunValue   = "Procyon Syncer"
)

func windowsStartupEnabled() (bool, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, windowsRunKeyPath, registry.QUERY_VALUE)
	if err == registry.ErrNotExist {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("odczyt autostartu Windows: %w", err)
	}
	defer key.Close()

	_, _, err = key.GetStringValue(windowsRunValue)
	if err == registry.ErrNotExist {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("odczyt autostartu Windows: %w", err)
	}
	return true, nil
}

func setWindowsStartupEnabled(enabled bool) error {
	if !enabled {
		key, err := registry.OpenKey(registry.CURRENT_USER, windowsRunKeyPath, registry.SET_VALUE)
		if err == registry.ErrNotExist {
			return nil
		}
		if err != nil {
			return fmt.Errorf("otwarcie autostartu Windows: %w", err)
		}
		defer key.Close()

		if err := key.DeleteValue(windowsRunValue); err != nil && err != registry.ErrNotExist {
			return fmt.Errorf("wyłączenie autostartu Windows: %w", err)
		}
		return nil
	}

	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("ustalenie ścieżki programu: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return fmt.Errorf("ustalenie pełnej ścieżki programu: %w", err)
	}

	key, _, err := registry.CreateKey(registry.CURRENT_USER, windowsRunKeyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("otwarcie autostartu Windows: %w", err)
	}
	defer key.Close()

	if err := key.SetStringValue(windowsRunValue, `"`+executable+`"`); err != nil {
		return fmt.Errorf("włączenie autostartu Windows: %w", err)
	}
	return nil
}
