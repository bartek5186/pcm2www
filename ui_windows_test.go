//go:build windows && !dev

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bartek5186/pcm2www/internal/appsettings"
	conf "github.com/bartek5186/pcm2www/internal/config"
	"github.com/bartek5186/pcm2www/internal/syncer"
	"github.com/lxn/walk"
	"github.com/lxn/win"
	"github.com/rs/zerolog"
)

// Requires an interactive Windows desktop. All files and settings are local
// fixtures; no integrations, shop requests or Windows startup changes occur.
func TestWindowsSettingsAndLogsStayIndependent(t *testing.T) {
	if os.Getenv("PCM2WWW_WINDOWS_UI_TESTS") != "1" {
		t.Skip("set PCM2WWW_WINDOWS_UI_TESTS=1 on an interactive Windows desktop")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	host, err := startWindowsUI(ctx, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg := &conf.Config{Database: conf.DBConfig{Driver: "sqlite"}}
	s := syncer.New(zerolog.Nop(), cfg, nil)
	var settings, logs *walk.Dialog
	entered, release, saved := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	onUI := func(fn func() error) {
		t.Helper()
		result := make(chan error, 1)
		host.Synchronize(func() { result <- fn() })
		select {
		case err := <-result:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("UI dispatcher blocked")
		}
	}
	onUI(func() error {
		var err error
		settings, err = showSettingsWindow(cfg, filepath.Join(dir, "config.json"), func(candidate *conf.Config) error {
			close(entered)
			<-release
			return appsettings.SaveAndApply(filepath.Join(dir, "config.json"), cfg, candidate, s)
		}, func(*conf.Config) { close(saved) })
		if err != nil {
			return err
		}
		button := findSaveButton(settings)
		if button == nil {
			return fmt.Errorf("save button missing")
		}
		win.SendMessage(button.Handle(), win.BM_CLICK, 0, 0)
		return nil
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("save did not start")
	}
	onUI(func() error {
		var err error
		logs, err = showLogWindow(filepath.Join(dir, "app.log"), s)
		if err != nil {
			return err
		}
		if settings.IsDisposed() || logs.IsDisposed() {
			return fmt.Errorf("opening logs closed another window")
		}
		return nil
	})
	close(release)
	select {
	case <-saved:
	case <-time.After(5 * time.Second):
		t.Fatal("save did not finish")
	}
	onUI(func() error {
		if !settings.IsDisposed() || logs.IsDisposed() || host.IsDisposed() || ctx.Err() != nil {
			return fmt.Errorf("save closed the UI/application instead of only settings")
		}
		logs.Cancel()
		var err error
		logs, err = showLogWindow(filepath.Join(dir, "app.log"), s)
		return err
	})
}

func findSaveButton(container walk.Container) *walk.PushButton {
	for i := 0; i < container.Children().Len(); i++ {
		widget := container.Children().At(i)
		if button, ok := widget.(*walk.PushButton); ok && button.Text() == "Zapisz i zastosuj" {
			return button
		}
		if child, ok := widget.(walk.Container); ok {
			if button := findSaveButton(child); button != nil {
				return button
			}
		}
	}
	return nil
}
