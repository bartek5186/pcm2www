//go:build windows && !dev

package main

import (
	"fmt"
	"strings"

	"github.com/bartek5186/pcm2www/internal/appsettings"
	conf "github.com/bartek5186/pcm2www/internal/config"
	"github.com/lxn/walk"
	ui "github.com/lxn/walk/declarative"
)

func showSettingsWindow(current *conf.Config, configPath string, apply func(*conf.Config) error, saved func(*conf.Config)) (*walk.Dialog, error) {
	values, err := appsettings.FromConfig(current)
	if err != nil {
		return nil, err
	}
	windowsStartup, startupReadErr := windowsStartupEnabled()
	initialStatus := ""
	if startupReadErr != nil {
		initialStatus = "Nie można odczytać ustawienia startu z Windowsem: " + startupReadErr.Error()
	}

	var (
		dialog         *walk.Dialog
		baseURLEdit    *walk.LineEdit
		consumerKey    *walk.LineEdit
		consumerSecret *walk.LineEdit
		showSecret     *walk.CheckBox
		wooPoll        *walk.NumberEdit
		wooWorkers     *walk.NumberEdit
		primeOnStart   *walk.CheckBox
		sweepMinutes   *walk.NumberEdit
		watchDir       *walk.LineEdit
		importPoll     *walk.NumberEdit
		stability      *walk.NumberEdit
		priceMode      *walk.ComboBox
		autoStart      *walk.CheckBox
		startAtLogin   *walk.CheckBox
		statusLabel    *walk.Label
		saveButton     *walk.PushButton
		cancelButton   *walk.PushButton
		wooEnabled     *walk.CheckBox
		importEnabled  *walk.CheckBox
		saving         bool
	)

	priceModeIndex := 0
	if strings.EqualFold(values.ImportPriceMode, "net") {
		priceModeIndex = 1
	}

	settingsDialog := ui.Dialog{
		AssignTo:      &dialog,
		Title:         "Procyon Syncer — ustawienia",
		Size:          ui.Size{Width: 760, Height: 690},
		MinSize:       ui.Size{Width: 720, Height: 670},
		FixedSize:     true,
		Font:          ui.Font{Family: "Segoe UI", PointSize: 9},
		DefaultButton: &saveButton,
		CancelButton:  &cancelButton,
		Layout: ui.VBox{
			Margins: ui.Margins{Left: 16, Top: 14, Right: 16, Bottom: 14},
			Spacing: 10,
		},
		Children: []ui.Widget{
			ui.Label{
				Text: "Połączenie PC-Market ↔ WooCommerce",
				Font: ui.Font{Family: "Segoe UI", PointSize: 13, Bold: true},
			},
			ui.Label{
				Text: "Niepełne ustawienia można zapisać przy zatrzymanej synchronizacji. Uruchamiane są tylko włączone integracje.",
			},
			ui.GroupBox{
				Title:  "WooCommerce",
				Layout: ui.Grid{Columns: 2, Spacing: 8},
				Children: []ui.Widget{
					ui.CheckBox{AssignTo: &wooEnabled, Text: "Włącz integrację WooCommerce", Checked: values.WooEnabled, ColumnSpan: 2},
					ui.Label{Text: "Adres sklepu:"},
					ui.LineEdit{
						AssignTo:  &baseURLEdit,
						Text:      values.WooBaseURL,
						CueBanner: "https://sklep.example.com",
					},
					ui.Label{Text: "Consumer key:"},
					ui.LineEdit{AssignTo: &consumerKey, Text: values.WooConsumerKey},
					ui.Label{Text: "Consumer secret:"},
					ui.Composite{
						Layout: ui.HBox{MarginsZero: true, Spacing: 8},
						Children: []ui.Widget{
							ui.LineEdit{
								AssignTo:      &consumerSecret,
								Text:          values.WooConsumerSecret,
								PasswordMode:  true,
								StretchFactor: 1,
							},
							ui.CheckBox{
								AssignTo: &showSecret,
								Text:     "Pokaż",
								OnCheckedChanged: func() {
									consumerSecret.SetPasswordMode(!showSecret.Checked())
								},
							},
						},
					},
					ui.Label{Text: "Workery / kolejka:"},
					ui.Composite{
						Layout: ui.HBox{MarginsZero: true, Spacing: 8},
						Children: []ui.Widget{
							ui.NumberEdit{
								AssignTo:           &wooWorkers,
								Value:              float64(values.WooWorkers),
								MinValue:           0,
								MaxValue:           32,
								MinSize:            ui.Size{Width: 90},
								MaxSize:            ui.Size{Width: 90},
								Decimals:           0,
								SpinButtonsVisible: true,
								ToolTipText:        "0 oznacza wartość domyślną: 3",
							},
							ui.Label{Text: "workerów; odpytywanie co"},
							ui.NumberEdit{
								AssignTo:           &wooPoll,
								Value:              float64(values.WooPollSeconds),
								MinValue:           0,
								MaxValue:           3600,
								MinSize:            ui.Size{Width: 90},
								MaxSize:            ui.Size{Width: 90},
								Decimals:           0,
								SpinButtonsVisible: true,
							},
							ui.Label{Text: "s"},
							ui.HSpacer{},
						},
					},
					ui.Label{Text: "Cache produktów:"},
					ui.Composite{
						Layout: ui.HBox{MarginsZero: true, Spacing: 8},
						Children: []ui.Widget{
							ui.CheckBox{
								AssignTo: &primeOnStart,
								Text:     "Pełne pobranie przy starcie",
								Checked:  values.WooPrimeOnStart,
							},
							ui.Label{Text: "Odświeżaj co"},
							ui.NumberEdit{
								AssignTo:           &sweepMinutes,
								Value:              float64(values.WooSweepIntervalMinutes),
								MinValue:           0,
								MaxValue:           10080,
								MinSize:            ui.Size{Width: 90},
								MaxSize:            ui.Size{Width: 90},
								Decimals:           0,
								SpinButtonsVisible: true,
							},
							ui.Label{Text: "min"},
							ui.HSpacer{},
						},
					},
				},
			},
			ui.GroupBox{
				Title:  "Import PC-Market",
				Layout: ui.Grid{Columns: 2, Spacing: 8},
				Children: []ui.Widget{
					ui.CheckBox{AssignTo: &importEnabled, Text: "Włącz import PC-Market", Checked: values.ImportEnabled, ColumnSpan: 2},
					ui.Label{Text: "Katalog plików XML:"},
					ui.Composite{
						Layout: ui.HBox{MarginsZero: true, Spacing: 8},
						Children: []ui.Widget{
							ui.LineEdit{AssignTo: &watchDir, Text: values.ImportWatchDir, StretchFactor: 1},
							ui.PushButton{
								Text: "Wybierz…",
								OnClicked: func() {
									picker := walk.FileDialog{
										Title:          "Wybierz katalog eksportów PC-Market",
										InitialDirPath: watchDir.Text(),
									}
									accepted, err := picker.ShowBrowseFolder(dialog)
									if err != nil {
										walk.MsgBox(dialog, "Wybór katalogu", err.Error(), walk.MsgBoxIconError)
										return
									}
									if accepted {
										watchDir.SetText(picker.FilePath)
									}
								},
							},
						},
					},
					ui.Label{Text: "Skanowanie / stabilność pliku:"},
					ui.Composite{
						Layout: ui.HBox{MarginsZero: true, Spacing: 8},
						Children: []ui.Widget{
							ui.Label{Text: "co"},
							ui.NumberEdit{
								AssignTo:           &importPoll,
								Value:              float64(values.ImportPollSeconds),
								MinValue:           0,
								MaxValue:           3600,
								MinSize:            ui.Size{Width: 90},
								MaxSize:            ui.Size{Width: 90},
								Decimals:           0,
								SpinButtonsVisible: true,
							},
							ui.Label{Text: "s; plik niezmienny przez"},
							ui.NumberEdit{
								AssignTo:           &stability,
								Value:              float64(values.ImportStabilitySeconds),
								MinValue:           0,
								MaxValue:           300,
								MinSize:            ui.Size{Width: 90},
								MaxSize:            ui.Size{Width: 90},
								Decimals:           0,
								SpinButtonsVisible: true,
							},
							ui.Label{Text: "s"},
							ui.HSpacer{},
						},
					},
					ui.Label{Text: "Ceny wysyłane do sklepu:"},
					ui.ComboBox{
						AssignTo:     &priceMode,
						Model:        []string{"Brutto — dolicz VAT do cen z XML", "Netto — ceny z XML bez przeliczenia"},
						CurrentIndex: priceModeIndex,
						ToolTipText:  "Eksport PC-Market zawiera ceny netto. Wybierz Brutto, jeśli w WooCommerce ceny są wpisywane z podatkiem.",
					},
				},
			},
			ui.GroupBox{
				Title:  "Uruchamianie",
				Layout: ui.VBox{Margins: ui.Margins{Left: 10, Top: 8, Right: 10, Bottom: 8}, Spacing: 6},
				Children: []ui.Widget{
					ui.CheckBox{
						AssignTo:    &startAtLogin,
						Text:        "Uruchamiaj Procyon Syncer po zalogowaniu do Windows",
						Checked:     windowsStartup,
						Enabled:     startupReadErr == nil,
						Alignment:   ui.AlignHNearVCenter,
						ToolTipText: "Dodaje program do autostartu tylko dla bieżącego użytkownika Windows.",
					},
					ui.CheckBox{
						AssignTo:  &autoStart,
						Text:      "Automatycznie uruchom synchronizację po otwarciu aplikacji",
						Checked:   values.AutoStart,
						Alignment: ui.AlignHNearVCenter,
					},
				},
			},
			ui.Label{AssignTo: &statusLabel, Text: initialStatus, TextColor: walk.RGB(170, 45, 35)},
			ui.Composite{
				Layout: ui.HBox{MarginsZero: true, Spacing: 8},
				Children: []ui.Widget{
					ui.PushButton{
						Text:        "Otwórz config.json…",
						ToolTipText: "Zamyka formularz i otwiera pełny plik konfiguracyjny.",
						OnClicked: func() {
							if saving {
								return
							}
							answer := walk.MsgBox(
								dialog,
								"Pełna konfiguracja",
								"Okno ustawień zostanie zamknięte bez zapisywania zmian. Po edycji config.json użyj w menu ikony opcji „Przeładuj konfigurację”.\n\nOtworzyć plik?",
								walk.MsgBoxYesNo|walk.MsgBoxIconQuestion,
							)
							if answer != walk.DlgCmdYes {
								return
							}
							if err := openInExplorer(configPath); err != nil {
								walk.MsgBox(dialog, "Nie można otworzyć config.json", err.Error(), walk.MsgBoxIconError)
								return
							}
							dialog.Cancel()
						},
					},
					ui.HSpacer{},
					ui.PushButton{
						AssignTo: &cancelButton,
						Text:     "Anuluj",
						MinSize:  ui.Size{Width: 100},
						OnClicked: func() {
							dialog.Cancel()
						},
					},
					ui.PushButton{
						AssignTo: &saveButton,
						Text:     "Zapisz i zastosuj",
						MinSize:  ui.Size{Width: 150},
						OnClicked: func() {
							saveButton.SetEnabled(false)
							statusLabel.SetText("Sprawdzanie i zapisywanie ustawień…")

							mode := "gross"
							if priceMode.CurrentIndex() == 1 {
								mode = "net"
							}
							candidate, err := appsettings.Apply(current, appsettings.Values{
								AutoStart:               autoStart.Checked(),
								WooEnabled:              wooEnabled.Checked(),
								ImportEnabled:           importEnabled.Checked(),
								WooBaseURL:              baseURLEdit.Text(),
								WooConsumerKey:          consumerKey.Text(),
								WooConsumerSecret:       consumerSecret.Text(),
								WooPollSeconds:          int(wooPoll.Value()),
								WooWorkers:              int(wooWorkers.Value()),
								WooPrimeOnStart:         primeOnStart.Checked(),
								WooSweepIntervalMinutes: int(sweepMinutes.Value()),
								ImportWatchDir:          watchDir.Text(),
								ImportPollSeconds:       int(importPoll.Value()),
								ImportStabilitySeconds:  int(stability.Value()),
								ImportPriceMode:         mode,
							})
							if err != nil {
								statusLabel.SetText("Nie zapisano ustawień: " + err.Error())
								saveButton.SetEnabled(true)
								return
							}
							saving = true
							dialog.SetEnabled(false)
							cancelButton.SetEnabled(false)
							requestedStartup := startAtLogin.Checked()
							startupChanged := startupReadErr == nil && requestedStartup != windowsStartup
							go func() {
								var applyErr error
								startupApplied := false
								defer func() {
									if r := recover(); r != nil {
										applyErr = fmt.Errorf("błąd zastosowania ustawień: %v", r)
									}
									if applyErr != nil && startupApplied {
										if rollbackErr := setWindowsStartupEnabled(windowsStartup); rollbackErr != nil {
											applyErr = fmt.Errorf("%v; przywrócenie autostartu: %w", applyErr, rollbackErr)
										}
									}
									dialog.Synchronize(func() {
										if dialog.IsDisposed() {
											return
										}
										saving = false
										dialog.SetEnabled(true)
										saveButton.SetEnabled(true)
										cancelButton.SetEnabled(true)
										if applyErr != nil {
											statusLabel.SetText("Nie zapisano ustawień: " + applyErr.Error())
											walk.MsgBox(dialog, "Nie można zapisać ustawień", applyErr.Error(), walk.MsgBoxIconError)
											return
										}
										saved(candidate)
										dialog.Accept()
									})
								}()
								if startupChanged {
									applyErr = setWindowsStartupEnabled(requestedStartup)
									startupApplied = applyErr == nil
								}
								if applyErr == nil {
									applyErr = apply(candidate)
								}
							}()
						},
					},
				},
			},
		},
	}

	if err := settingsDialog.Create(nil); err != nil {
		return nil, fmt.Errorf("tworzenie okna ustawień: %w", err)
	}
	dialog.Closing().Attach(func(cancel *bool, _ walk.CloseReason) {
		if saving {
			*cancel = true
		}
	})
	showModelessDialog(dialog)
	return dialog, nil
}
