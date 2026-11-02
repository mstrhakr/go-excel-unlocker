//go:build windows

package main

import (
	"errors"
	"path/filepath"
	"strings"

	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/win"
	"github.com/rodrigocfd/windigo/x/cosh"
	"github.com/rodrigocfd/windigo/x/winsh"
)

// An empty input opens a single-file picker; otherwise open Save As.
func pickWorkbook(owner win.HWND, input string) (string, error) {
	releaser := win.NewOleReleaser()
	defer releaser.Release()
	var dialog *winsh.IFileDialog
	options := cosh.FOS_FORCEFILESYSTEM | cosh.FOS_PATHMUSTEXIST | cosh.FOS_NOCHANGEDIR
	if input == "" {
		var open *winsh.IFileOpenDialog
		if err := win.CoCreateInstance(releaser, &cosh.CLSID_FileOpenDialog, nil, co.CLSCTX_INPROC_SERVER, &open); err != nil {
			return "", err
		}
		dialog = &open.IFileDialog
		options |= cosh.FOS_FILEMUSTEXIST
		if err := dialog.SetTitle("Choose one Excel workbook"); err != nil {
			return "", err
		}
	} else {
		var save *winsh.IFileSaveDialog
		if err := win.CoCreateInstance(releaser, &cosh.CLSID_FileSaveDialog, nil, co.CLSCTX_INPROC_SERVER, &save); err != nil {
			return "", err
		}
		dialog = &save.IFileDialog
		// Replacement is explicitly confirmed in our UI, then passed to the
		// engine. Never grant replace permission merely because Save was clicked.
		ext := filepath.Ext(input)
		name := strings.TrimSuffix(filepath.Base(input), ext) + " - unprotected" + ext
		if err := dialog.SetFileName(name); err != nil {
			return "", err
		}
		if err := dialog.SetDefaultExtension(strings.TrimPrefix(ext, ".")); err != nil {
			return "", err
		}
		var folder *winsh.IShellItem
		if err := winsh.SHCreateItemFromParsingName(releaser, filepath.Dir(input), &folder); err != nil {
			return "", err
		}
		if err := dialog.SetFolder(folder); err != nil {
			return "", err
		}
		if err := dialog.SetTitle("Save unlocked copy"); err != nil {
			return "", err
		}
	}
	if err := dialog.SetOptions(options); err != nil {
		return "", err
	}
	filters := []winsh.COMDLG_FILTERSPEC{{Name: "Excel workbooks", Spec: "*.xlsx;*.xlsm"}}
	if input != "" {
		ext := strings.ToLower(filepath.Ext(input))
		filters = []winsh.COMDLG_FILTERSPEC{{Name: "Excel workbook (" + ext + ")", Spec: "*" + ext}}
	}
	if err := dialog.SetFileTypes(filters); err != nil {
		return "", err
	}
	ok, err := dialog.Show(owner)
	// Windigo 0.2.7 compares Show's cancellation HRESULT to a Win32 error.
	if errors.Is(err, co.ERROR_CANCELLED.ToHresult()) {
		return "", nil
	}
	if err != nil || !ok {
		return "", err
	}
	item, err := dialog.GetResult(releaser)
	if err != nil {
		return "", err
	}
	return item.GetDisplayName(cosh.SIGDN_FILESYSPATH)
}
