//go:build windows && !dev

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/bartek5186/pcm2www/internal/problems"
	"github.com/bartek5186/pcm2www/internal/syncer"
	"github.com/lxn/walk"
	ui "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
	"golang.org/x/sys/windows"
	"gorm.io/gorm"
)

type problemTableModel struct {
	walk.TableModelBase
	rows []problems.Row
}

func (m *problemTableModel) RowCount() int { return len(m.rows) }
func (m *problemTableModel) Value(row, column int) interface{} {
	r := m.rows[row]
	switch column {
	case 0:
		return r.Category
	case 1:
		return r.Problem
	case 2:
		if r.WooName != "" {
			return r.WooName
		}
		return r.PCMName
	case 3:
		if r.PCMCode != "" {
			return r.PCMCode
		}
		return r.WooEAN
	case 4:
		if r.PCMID != 0 {
			return strconv.FormatInt(r.PCMID, 10)
		}
	case 5:
		if r.WooID != 0 {
			return strconv.FormatUint(uint64(r.WooID), 10)
		}
	}
	return ""
}

func showProblemsWindow(parent context.Context, gdb *gorm.DB, baseURL string, s *syncer.Syncer) (*walk.Dialog, error) {
	ctx, cancel := context.WithCancel(parent)
	var closed atomic.Bool
	var dialog *walk.Dialog
	var table *walk.TableView
	var query *walk.LineEdit
	var category *walk.ComboBox
	var summary *walk.Label
	var details *walk.TextEdit
	var refresh, export, open, edit, closeButton *walk.PushButton
	model := &problemTableModel{}
	var snapshot problems.Snapshot
	loading := false
	categories := []string{"Wszystkie", "EAN", "Synchronizacja", "Import", "Integracja"}
	reportError := func(err error) { walk.MsgBox(dialog, "Raport problemów", err.Error(), walk.MsgBoxIconError) }
	selection := func() *problems.Row {
		idx := table.CurrentIndex()
		if idx < 0 || idx >= len(model.rows) {
			return nil
		}
		return &model.rows[idx]
	}
	updateSelection := func() {
		if details == nil || open == nil || edit == nil {
			return
		}
		r := selection()
		open.SetEnabled(r != nil && r.ProductURL != "")
		edit.SetEnabled(r != nil && r.EditURL != "")
		if r == nil {
			_ = details.SetText("")
			return
		}
		_ = details.SetText(fmt.Sprintf("%s\r\n%s\r\nPCM: %s | kod: %s | ID: %d\r\nWoo: %s | EAN: %s | ID: %d\r\nOperacja: %s | zadanie: %d | import: %d | plik: %s\r\n%s\r\n%s", r.Problem, r.Details, r.PCMName, r.PCMCode, r.PCMID, r.WooName, r.WooEAN, r.WooID, r.Operation, r.TaskID, r.ImportID, r.File, r.ProductURL, r.EditURL))
	}
	applyFilter := func() {
		if table == nil || query == nil || category == nil || summary == nil {
			return
		}
		cat := ""
		if idx := category.CurrentIndex(); idx > 0 {
			cat = categories[idx]
		}
		model.rows = problems.Filter(snapshot.Rows, cat, query.Text())
		model.PublishRowsReset()
		_ = table.SetCurrentIndex(-1)
		updateSelection()
		if !snapshot.At.IsZero() {
			text := fmt.Sprintf("Stan lokalny: %s | widoczne: %d / %d | PCM: %d | Woo: %d", snapshot.At.Format("02.01.2006 15:04:05"), len(model.rows), len(snapshot.Rows), snapshot.PCMCount, snapshot.WooCount)
			if snapshot.WooCount == 0 || snapshot.PCMCount == 0 {
				text += " — dane niepełne; uruchom synchronizację"
			}
			_ = summary.SetText(text)
		}
	}
	load := func() {
		if loading {
			return
		}
		loading = true
		refresh.SetEnabled(false)
		export.SetEnabled(false)
		query.SetEnabled(false)
		category.SetEnabled(false)
		_ = summary.SetText("Odczytywanie bieżących problemów…")
		go func() {
			readCtx, stop := context.WithTimeout(ctx, 30*time.Second)
			defer stop()
			result, err := problems.Load(readCtx, gdb, baseURL)
			if err == nil {
				for _, st := range s.IntegrationStatuses() {
					if st.State == "failed" {
						result.Rows = append(result.Rows, problems.Row{Category: "Integracja", Code: "integration_error", Problem: "Błąd integracji: " + st.Name, Details: st.LastError})
					}
				}
			}
			if closed.Load() {
				return
			}
			dialog.Synchronize(func() {
				if closed.Load() {
					return
				}
				loading = false
				refresh.SetEnabled(true)
				if err != nil {
					_ = summary.SetText("Nie udało się odświeżyć danych. Poprzedni widok nie jest aktualny.")
					reportError(err)
					return
				}
				snapshot = result
				query.SetEnabled(true)
				category.SetEnabled(true)
				applyFilter()
				export.SetEnabled(true)
			})
		}()
	}
	openURL := func(admin bool) {
		r := selection()
		if r == nil {
			return
		}
		target := r.ProductURL
		if admin {
			target = r.EditURL
		}
		if target == "" {
			return
		}
		ptr, err := windows.UTF16PtrFromString(target)
		if err == nil {
			err = windows.ShellExecute(windows.Handle(dialog.Handle()), nil, ptr, nil, nil, windows.SW_SHOWNORMAL)
		}
		if err != nil {
			reportError(fmt.Errorf("otwarcie produktu: %w", err))
		}
	}
	save := func() {
		picker := walk.FileDialog{Title: "Zapisz widoczne problemy jako CSV", FilePath: "problemy_" + snapshot.At.Format("20060102_150405") + ".csv", Filter: "Pliki CSV (*.csv)|*.csv", Flags: win.OFN_OVERWRITEPROMPT}
		ok, err := picker.ShowSave(dialog)
		if err != nil {
			reportError(err)
			return
		}
		if !ok {
			return
		}
		path := picker.FilePath
		if filepath.Ext(path) == "" {
			path += ".csv"
			if _, statErr := os.Stat(path); statErr == nil {
				if walk.MsgBox(dialog, "Zastąpić plik?", path+" już istnieje. Zastąpić go?", walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) != walk.DlgCmdYes {
					return
				}
			} else if !os.IsNotExist(statErr) {
				reportError(statErr)
				return
			}
		}
		// Write a temporary file first, so a failed export cannot truncate an
		// existing report. The save dialog confirms replacing its exact path.
		file, err := os.CreateTemp(filepath.Dir(path), ".problemy-*.csv")
		if err != nil {
			reportError(err)
			return
		}
		tmp := file.Name()
		defer os.Remove(tmp)
		err = problems.WriteCSV(file, snapshot.At, model.rows)
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Rename(tmp, path)
		}
		if err != nil {
			reportError(fmt.Errorf("zapis CSV: %w", err))
			return
		}
		walk.MsgBox(dialog, "Raport zapisany", fmt.Sprintf("Zapisano %d wierszy:\n%s\n\nW Excelu importuj kolumny EAN/kod jako tekst, aby zachować zera na początku.", len(model.rows), path), walk.MsgBoxIconInformation)
	}
	dlg := ui.Dialog{AssignTo: &dialog, Title: "Procyon Syncer — Analityka", Size: ui.Size{Width: 1150, Height: 720}, MinSize: ui.Size{Width: 850, Height: 550}, Font: ui.Font{Family: "Segoe UI", PointSize: 9}, CancelButton: &closeButton, Layout: ui.VBox{}, Children: []ui.Widget{
		ui.Label{Text: "Analityka", Font: ui.Font{PointSize: 13, Bold: true}},
		ui.Label{Text: "Dane z lokalnej bazy. Odśwież pobiera bieżący stan, a eksport zapisuje widoczne wiersze. Dwuklik otwiera produkt w sklepie."},
		ui.Composite{Layout: ui.HBox{MarginsZero: true}, Children: []ui.Widget{
			ui.ComboBox{AssignTo: &category, Model: categories, CurrentIndex: 0, OnCurrentIndexChanged: applyFilter, MinSize: ui.Size{Width: 150}},
			ui.LineEdit{AssignTo: &query, CueBanner: "Szukaj: nazwa, EAN, ID, plik lub błąd…", OnTextChanged: applyFilter, StretchFactor: 1},
			ui.PushButton{AssignTo: &refresh, Text: "Odśwież", OnClicked: load},
			ui.PushButton{AssignTo: &export, Text: "Eksportuj CSV…", Enabled: false, OnClicked: save},
		}},
		ui.Label{AssignTo: &summary, Text: "Oczekiwanie na odczyt…"},
		ui.TableView{AssignTo: &table, Model: model, StretchFactor: 1, AlternatingRowBG: true, LastColumnStretched: true, ColumnsSizable: true, NotSortableByHeaderClick: true, OnCurrentIndexChanged: updateSelection, OnItemActivated: func() { openURL(false) }, Columns: []ui.TableViewColumn{
			{Title: "Kategoria", Width: 110}, {Title: "Problem", Width: 250}, {Title: "Produkt", Width: 300}, {Title: "EAN / kod", Width: 145}, {Title: "ID PCM", Width: 85}, {Title: "ID Woo", Width: 75},
		}},
		ui.TextEdit{AssignTo: &details, ReadOnly: true, VScroll: true, MinSize: ui.Size{Height: 140}, MaxSize: ui.Size{Height: 160}},
		ui.Composite{Layout: ui.HBox{MarginsZero: true}, Children: []ui.Widget{
			ui.PushButton{AssignTo: &open, Text: "Otwórz produkt", Enabled: false, OnClicked: func() { openURL(false) }},
			ui.PushButton{AssignTo: &edit, Text: "Edytuj w WooCommerce", Enabled: false, OnClicked: func() { openURL(true) }},
			ui.HSpacer{}, ui.PushButton{AssignTo: &closeButton, Text: "Zamknij", OnClicked: func() { dialog.Cancel() }},
		}},
	}}
	if err := dlg.Create(nil); err != nil {
		cancel()
		return nil, err
	}
	dialog.Disposing().Once(func() { closed.Store(true); cancel() })
	showModelessDialog(dialog)
	load()
	return dialog, nil
}
