//go:build windows && !dev

package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bartek5186/pcm2www/internal/logview"
	"github.com/bartek5186/pcm2www/internal/syncer"
	"github.com/lxn/walk"
	ui "github.com/lxn/walk/declarative"
)

const logViewerMaxBytes = 512 << 10

func showLogWindow(logPath string, s *syncer.Syncer) error {
	initialStatus := s.Status()
	indicatorColor, _ := statusAppearance(initialStatus.State)
	reader := logview.NewReader(logPath, logViewerMaxBytes)
	initialText, _, initialErr := reader.Read()
	if initialText == "" && initialErr == nil {
		initialText = "Brak wpisów w logu."
	}

	statusText := "Na żywo — oczekiwanie na nowe wpisy"
	statusColor := walk.RGB(90, 95, 105)
	if initialErr != nil {
		statusText = initialErr.Error()
		statusColor = walk.RGB(170, 45, 35)
	}

	var (
		dialog      *walk.Dialog
		logText     *walk.TextEdit
		statusLabel *walk.Label
		stateLabel  *walk.Label
		stateLamp   *walk.Label
		closeButton *walk.PushButton
	)

	logDialog := ui.Dialog{
		AssignTo:     &dialog,
		Title:        "Procyon Syncer — logi na żywo",
		Size:         ui.Size{Width: 960, Height: 620},
		MinSize:      ui.Size{Width: 720, Height: 420},
		CancelButton: &closeButton,
		Font:         ui.Font{Family: "Segoe UI", PointSize: 9},
		Layout: ui.VBox{
			Margins: ui.Margins{Left: 14, Top: 14, Right: 14, Bottom: 14},
			Spacing: 8,
		},
		Children: []ui.Widget{
			ui.Label{
				Text: "Ostatnie logi aplikacji",
				Font: ui.Font{Family: "Segoe UI", PointSize: 13, Bold: true},
			},
			ui.Composite{
				Layout: ui.HBox{MarginsZero: true, Spacing: 8},
				Children: []ui.Widget{
					ui.Label{AssignTo: &stateLamp, Text: "●", TextColor: indicatorColor, Font: ui.Font{Family: "Segoe UI Symbol", PointSize: 14}, ToolTipText: initialStatus.Detail},
					ui.Label{AssignTo: &stateLabel, Text: initialStatus.Text, TextColor: indicatorColor, Font: ui.Font{Family: "Segoe UI", PointSize: 10, Bold: true}, ToolTipText: initialStatus.Detail},
					ui.HSpacer{},
				},
			},
			ui.TextEdit{
				AssignTo:      &logText,
				Text:          initialText,
				ReadOnly:      true,
				HScroll:       true,
				VScroll:       true,
				MaxLength:     logViewerMaxBytes * 2,
				StretchFactor: 1,
				Font:          ui.Font{Family: "Consolas", PointSize: 9},
			},
			ui.Composite{
				Layout: ui.HBox{MarginsZero: true, Spacing: 8},
				Children: []ui.Widget{
					ui.Label{AssignTo: &statusLabel, Text: statusText, TextColor: statusColor},
					ui.HSpacer{},
					ui.PushButton{
						AssignTo: &closeButton,
						Text:     "Zamknij",
						MinSize:  ui.Size{Width: 100},
						OnClicked: func() {
							dialog.Cancel()
						},
					},
				},
			},
		},
	}

	if err := logDialog.Create(nil); err != nil {
		return fmt.Errorf("tworzenie okna logów: %w", err)
	}
	if icon, iconErr := walk.NewIconFromResourceId(2); iconErr == nil {
		defer icon.Dispose()
		_ = dialog.SetIcon(icon)
	}
	scrollLogToEnd(logText)

	done := make(chan struct{})
	var stopOnce sync.Once
	var closed atomic.Bool
	stop := func() {
		closed.Store(true)
		stopOnce.Do(func() { close(done) })
	}
	dialog.Closing().Once(func(_ *bool, _ walk.CloseReason) { stop() })

	go func() {
		lastStatus := initialStatus
		ticker := time.NewTicker(750 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				text, changed, err := reader.Read()
				currentStatus := s.Status()
				if !changed && err == nil && currentStatus == lastStatus {
					continue
				}
				lastStatus = currentStatus
				updatedText, updateErr := text, err
				dialog.Synchronize(func() {
					if closed.Load() {
						return
					}
					color, _ := statusAppearance(currentStatus.State)
					_ = stateLabel.SetText(currentStatus.Text)
					stateLabel.SetTextColor(color)
					stateLamp.SetTextColor(color)
					_ = stateLabel.SetToolTipText(currentStatus.Detail)
					_ = stateLamp.SetToolTipText(currentStatus.Detail)
					if updateErr != nil {
						statusLabel.SetText(updateErr.Error())
						statusLabel.SetTextColor(walk.RGB(170, 45, 35))
						return
					}
					if !changed {
						return
					}
					if updatedText == "" {
						updatedText = "Brak wpisów w logu."
					}
					_ = logText.SetText(updatedText)
					scrollLogToEnd(logText)
					statusLabel.SetText("Na żywo — zaktualizowano o " + time.Now().Format("15:04:05"))
					statusLabel.SetTextColor(walk.RGB(90, 95, 105))
				})
			}
		}
	}()

	dialog.Run()
	stop()
	return nil
}

func scrollLogToEnd(text *walk.TextEdit) {
	end := text.TextLength()
	text.SetTextSelection(end, end)
	text.ScrollToCaret()
}
