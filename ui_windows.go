//go:build windows && !dev

package main

import (
	"context"
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/lxn/walk"
	"github.com/lxn/win"
	"github.com/rs/zerolog"
)

// All Walk windows share one persistent OS thread and one message loop.
// systray runs its own loop; opening/closing a dialog never owns that lifecycle.
func startWindowsUI(ctx context.Context, log zerolog.Logger) (*walk.MainWindow, error) {
	type result struct {
		host *walk.MainWindow
		err  error
	}
	ready := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		walk.App().Panicking().Attach(func(err error) {
			log.Error().Err(err).Str("stack", string(debug.Stack())).Msg("Błąd interfejsu Windows")
			messageBox("Procyon Syncer — błąd okna", "Wystąpił błąd interfejsu. Szczegóły zapisano w app.log.\n"+err.Error())
		})
		host, err := walk.NewMainWindow()
		if err != nil {
			ready <- result{err: err}
			return
		}
		ready <- result{host: host}
		go func() {
			<-ctx.Done()
			host.Synchronize(func() {
				for dialog := range openUIDialogs {
					dialog.Dispose()
				}
				host.Dispose()
			})
		}()
		host.Run() // host stays hidden; only the requested dialogs are shown
	}()
	r := <-ready
	return r.host, r.err
}

// Accessed only on the Walk thread.
var openUIDialogs = make(map[*walk.Dialog]struct{})

func showModelessDialog(dialog *walk.Dialog) {
	openUIDialogs[dialog] = struct{}{}
	dialog.Disposing().Once(func() { delete(openUIDialogs, dialog) })
	if icon, err := walk.NewIconFromResourceId(2); err == nil {
		_ = dialog.SetIcon(icon)
		dialog.Disposing().Once(func() { icon.Dispose() })
	}
	// The hidden host owns the loop. Route dialog-navigation keys to the
	// actual focused dialog, preserving Tab, Enter and Escape in every window.
	for _, shortcut := range []walk.Shortcut{
		{Key: walk.KeyTab}, {Key: walk.KeyTab, Modifiers: walk.ModShift}, {Key: walk.KeyReturn}, {Key: walk.KeyEscape},
	} {
		action := walk.NewAction()
		_ = action.SetShortcut(shortcut)
		action.Triggered().Attach(func() {
			msg := win.MSG{HWnd: win.GetFocus(), Message: win.WM_KEYDOWN, WParam: uintptr(shortcut.Key)}
			if !win.IsDialogMessage(dialog.Handle(), &msg) {
				win.TranslateMessage(&msg)
				win.DispatchMessage(&msg)
			}
		})
		_ = dialog.ShortcutActions().Add(action)
	}
	dialog.Show()
}

func raiseDialog(dialog *walk.Dialog) bool {
	if dialog == nil || dialog.IsDisposed() {
		return false
	}
	win.ShowWindow(dialog.Handle(), win.SW_RESTORE)
	win.SetForegroundWindow(dialog.Handle())
	return true
}

func dispatchUI(host *walk.MainWindow, log zerolog.Logger, fn func()) {
	host.Synchronize(func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error().Str("stack", string(debug.Stack())).Msgf("Błąd obsługi okna: %v", r)
				messageBox("Procyon Syncer — błąd okna", fmt.Sprint(r))
			}
		}()
		fn()
	})
}
