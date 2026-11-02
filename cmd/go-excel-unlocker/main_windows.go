//go:build windows

// Build without a console: go build -ldflags="-H=windowsgui" ./cmd/go-excel-unlocker
//
// The checked-in Windows resources embed app.manifest (no third-party artwork).
// To regenerate from this directory with LLVM:
// llvm-windres-18 --target=pe-x86-64 --input=app.rc --output=resource_windows_amd64.syso
// llvm-windres-18 --input=app.rc --output-format=res --output=app.res
// llvm-cvtres-18 /MACHINE:ARM64 /TIMESTAMP:0 /OUT:resource_windows_arm64.syso app.res
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/mstrhakr/go-excel-unlocker/internal/unlock"
	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/ui"
	"github.com/rodrigocfd/windigo/win"
)

const pollTimer = 1

type result struct {
	info   unlock.Info
	err    error
	output string
}

type application struct {
	window   *ui.Main
	pathText *ui.Edit
	status   *ui.Static
	progress *ui.ProgressBar
	browse   *ui.Button
	unlock   *ui.Button
	cancel   *ui.Button
	path     string
	ready    bool
	busy     bool
	closing  bool
	stopping bool
	stop     context.CancelFunc
	stages   chan string
	done     chan result
}

func main() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer func() {
		if failure := recover(); failure != nil {
			_, _ = win.HWND(0).MessageBox(
				fmt.Sprintf("The application could not continue: %v\n\nClose other applications and try again.", failure),
				"Excel Unlocker", co.MB_OK|co.MB_ICONERROR)
		}
	}()
	if _, err := win.CoInitializeEx(co.COINIT_APARTMENTTHREADED | co.COINIT_DISABLE_OLE1DDE); err != nil {
		panic(err)
	}
	defer win.CoUninitialize()
	newApplication().window.RunAsMain()
}

func newApplication() *application {
	a := &application{
		stages: make(chan string, 16),
		done:   make(chan result, 1),
		window: ui.NewMain(ui.OptsMain().Title("Excel Unlocker").
			Center(true).Size(ui.Dpi(660, 375)).ExStyle(co.WS_EX_ACCEPTFILES)),
	}
	ui.NewStatic(a.window, ui.OptsStatic().Text("Drop one .xlsx or .xlsm workbook here, or choose Browse.").
		Position(ui.Dpi(20, 18)).Size(ui.Dpi(620, 25)).CtrlStyle(co.SS_LEFT|co.SS_NOPREFIX))
	a.pathText = ui.NewEdit(a.window, ui.OptsEdit().Text("No workbook selected").
		Position(ui.Dpi(20, 50)).Width(ui.DpiX(510)).Height(ui.DpiY(28)).
		CtrlStyle(co.ES_READONLY|co.ES_AUTOHSCROLL))
	a.browse = ui.NewButton(a.window, ui.OptsButton().Text("&Browse…").
		Position(ui.Dpi(540, 50)).Width(ui.DpiX(100)).Height(ui.DpiY(28)))
	ui.NewStatic(a.window, ui.OptsStatic().Text(unlock.Limitation).
		Position(ui.Dpi(20, 92)).Size(ui.Dpi(620, 78)).CtrlStyle(co.SS_LEFT|co.SS_NOPREFIX))
	a.status = ui.NewStatic(a.window, ui.OptsStatic().Text("Select a workbook to inspect its protection.").
		Position(ui.Dpi(20, 182)).Size(ui.Dpi(620, 74)).CtrlStyle(co.SS_LEFT|co.SS_NOPREFIX))
	a.progress = ui.NewProgressBar(a.window, ui.OptsProgressBar().
		Position(ui.Dpi(20, 265)).Size(ui.Dpi(620, 20)))
	a.unlock = ui.NewButton(a.window, ui.OptsButton().Text("&Unlock / Save As…").
		Position(ui.Dpi(20, 305)).Width(ui.DpiX(180)).Height(ui.DpiY(32)))
	a.cancel = ui.NewButton(a.window, ui.OptsButton().Text("&Cancel").
		Position(ui.Dpi(210, 305)).Width(ui.DpiX(100)).Height(ui.DpiY(32)))
	a.window.On().WmCreate(func(_ ui.WmCreate) int {
		a.controls()
		if err := a.window.Hwnd().SetTimer(pollTimer, 75); err != nil {
			panic(err)
		}
		return 0
	})
	a.window.On().WmTimer(pollTimer, a.poll)
	a.window.On().WmClose(a.close)
	a.window.On().WmDropFiles(func(p ui.WmDropFiles) {
		drop := p.HDrop()
		defer drop.DragFinish()
		if a.busy || a.closing {
			return
		}
		paths, err := drop.DragQueryFile()
		if err != nil {
			a.showError("Could not read the dropped file", err)
		} else if len(paths) != 1 {
			a.message("Please drop exactly one workbook at a time.", co.MB_ICONINFORMATION)
		} else {
			a.selectPath(paths[0])
		}
	})
	a.browse.On().BnClicked(func() {
		if a.busy || a.closing {
			return
		}
		path, err := pickWorkbook(a.window.Hwnd(), "")
		if err != nil {
			a.showError("Could not open the file picker", err)
		} else if path != "" {
			a.selectPath(path)
		}
	})
	a.unlock.On().BnClicked(a.save)
	a.cancel.On().BnClicked(a.cancelWork)
	return a
}

func (a *application) text(s string) { _ = a.status.Hwnd().SetWindowText(s) }

func (a *application) message(s string, icon co.MB) {
	_, _ = a.window.Hwnd().MessageBox(s, "Excel Unlocker", co.MB_OK|icon)
}

func (a *application) showError(action string, err error) {
	a.text(action + ".")
	a.message(fmt.Sprintf("%s:\n\n%v\n\nChoose a supported, unencrypted workbook. Close Excel if it is using the file, and check that you have permission to read the input and write to the destination.", action, err), co.MB_ICONERROR)
}

func (a *application) controls() {
	a.browse.Hwnd().EnableWindow(!a.busy && !a.closing)
	a.unlock.Hwnd().EnableWindow(a.ready && !a.busy && !a.closing)
	a.cancel.Hwnd().EnableWindow(a.busy && !a.stopping)
}

func (a *application) begin(status string) context.Context {
	ctx, stop := context.WithCancel(context.Background())
	a.stop, a.busy, a.stopping = stop, true, false
	a.text(status)
	a.progress.SetMarquee(true)
	a.controls()
	return ctx
}

func (a *application) selectPath(path string) {
	if a.busy || a.closing {
		return
	}
	a.path, a.ready = path, false
	a.pathText.SetText(path)
	ctx := a.begin("Inspecting workbook… (indeterminate)")
	go func() {
		info, err := unlock.Inspect(ctx, path)
		a.done <- result{info: info, err: err}
	}()
}

func (a *application) save() {
	if a.busy || a.closing || !a.ready {
		return
	}
	output, err := pickWorkbook(a.window.Hwnd(), a.path)
	if err != nil {
		a.showError("Could not choose an output file", err)
		return
	}
	if output == "" {
		return
	}
	// Input identity (including hard links) is also checked by the engine.
	if strings.EqualFold(filepath.Clean(a.path), filepath.Clean(output)) {
		a.message("Choose a different output filename. The original workbook must not be overwritten.", co.MB_ICONWARNING)
		return
	}
	replace := false
	if _, err := os.Lstat(output); err == nil {
		answer, dialogErr := a.window.Hwnd().MessageBox(
			"Replace this existing file?\n\n"+output+"\n\nThis cannot be undone.",
			"Confirm replacement", co.MB_YESNO|co.MB_ICONWARNING|co.MB_DEFBUTTON2)
		if dialogErr != nil || answer != co.ID_YES {
			return
		}
		replace = true
	} else if !errors.Is(err, os.ErrNotExist) {
		a.showError("Could not check the destination", err)
		return
	}
	input := a.path
	ctx := a.begin("Starting unlock… (indeterminate)")
	go func() {
		info, err := unlock.Unlock(ctx, input, output, replace, func(stage string) {
			select {
			case a.stages <- stage:
			case <-ctx.Done():
			}
		})
		a.done <- result{info: info, err: err, output: output}
	}()
}

func (a *application) cancelWork() {
	if !a.busy || a.stopping {
		return
	}
	a.stopping = true
	a.stop()
	a.text("Cancelling… waiting for safe cleanup.")
	a.controls()
}

func (a *application) close() {
	a.closing = true
	if a.busy {
		a.cancelWork()
		a.controls()
		return
	}
	_ = a.window.Hwnd().KillTimer(pollTimer)
	_ = a.window.Hwnd().DestroyWindow()
}

// Workers send data only. The UI timer consumes it on the window thread; no
// worker can send a message to a destroyed HWND, even while the window closes.
func (a *application) poll() {
	for {
		select {
		case stage := <-a.stages:
			if !a.stopping {
				a.text(stage + "… (indeterminate)")
			}
		default:
			goto completion
		}
	}
completion:
	select {
	case r := <-a.done:
		// Completion guarantees the stage producer has exited. Discard any
		// stage that arrived between the first drain and this receive.
		for len(a.stages) != 0 {
			<-a.stages
		}
		a.stop()
		a.stop, a.busy = nil, false
		a.progress.SetMarquee(false).SetPos(0)
		if a.closing {
			a.close()
			return
		}
		a.controls()
		switch {
		case errors.Is(r.err, context.Canceled):
			a.text("Cancelled. Any incomplete output has been cleaned up.")
		case r.err != nil:
			a.showError("Workbook operation failed", r.err)
		case r.output != "":
			a.progress.SetPos(100)
			a.text("Success! Saved to: " + r.output)
			a.message("Unlocked copy saved successfully:\n\n"+r.output+"\n\nThe original workbook was not changed.", co.MB_ICONINFORMATION)
		default:
			a.ready = r.info.Protected()
			if a.ready {
				a.text(fmt.Sprintf("Ready: %d protected worksheet(s); workbook protection: %t; macros: %t.\nChoose Unlock / Save As to create a separate copy.", r.info.Sheets, r.info.Workbook, r.info.MacroEnabled))
			} else {
				a.text("No worksheet or workbook protection was found. No unlocked copy is needed.")
			}
			a.controls()
		}
	default:
	}
}
