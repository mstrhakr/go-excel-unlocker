# go-excel-unlocker

A small, Windows-only native desktop application for removing worksheet and
workbook structure protection from `.xlsx` and `.xlsm` files. Single-file
workflow; all processing stays on your computer. No uploads, telemetry,
external services, password cracking, or macro execution.

> This tool removes worksheet and workbook protection. It cannot remove
> password-to-open encryption. Encrypted files require the password in Excel.

## Usage

1. Download `go-excel-unlocker.exe` from
   [Releases](https://github.com/mstrhakr/go-excel-unlocker/releases), or build it below.
2. Drag one workbook onto the window, or select **Browse**.
3. Review the selected path and detected protection, then select **Unlock**.
4. Choose a destination in the native Save As dialog. The default name is
   `<original name> - unprotected.xlsx` (or `.xlsm`).
5. Wait for validation and the explicit success message, then open the copy in Excel.

The original is never overwritten, including when the destination is another
path to the same file. Replacing another existing file requires confirmation.
Already-unprotected workbooks are reported honestly; no “unlocked” copy is made.
Inspection and processing run off the UI thread, with real stage messages and an
indeterminate activity indicator rather than estimated percentages.

If processing fails, read the displayed reason. For password-to-open encryption,
open the file using its password in Excel and save an unencrypted OOXML copy
first. Legacy `.xls` and other unsupported formats must be converted in Excel.
OLE compound files are refused because they may be encrypted OOXML **or** an
unsupported legacy format; they are not all assumed to be encrypted.

## Preservation and safety

The engine edits ZIP/XML directly, removing only applicable `sheetProtection`
and `workbookProtection` elements without reserializing entire XML documents.
All unrelated archive entries retain their uncompressed bytes, including VBA,
formulas, styles, relationships, and embedded objects. VBA project protection is
not removed. ZIP container metadata/compression and the changed XML parts are
not guaranteed byte-identical.

File signatures, OOXML structure, XML, and the final output are validated.
Encrypted ZIP entries, signed Office packages (edits would invalidate their
signatures), malformed files, and archives exceeding resource limits are refused.
Archive paths are never extracted to disk. A temporary file in the destination
directory is validated before finalization and cleaned up on failure or cancellation.
This tool does not evaluate formulas or remove other access controls.

Keep backups and review important outputs in Excel. The executable is not
code-signed; Windows may display SmartScreen warnings. Only process files you
are authorized to modify.

## Build and test

Requires Go 1.24 or newer. The UI uses
[Windigo](https://github.com/rodrigocfd/windigo), a pure-Go native Win32 library:
its native file-drop events, common file dialogs, and UI-thread dispatch avoid a
browser runtime or CGo dependency.

From the repository root in PowerShell:

```powershell
go mod download
go test -race ./...
go vet ./...
go build -trimpath -ldflags="-s -w -H=windowsgui" -o bin/go-excel-unlocker.exe ./cmd/go-excel-unlocker
```

Cross-build from Linux:

```sh
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-s -w -H=windowsgui" -o bin/go-excel-unlocker.exe ./cmd/go-excel-unlocker
GOOS=windows GOARCH=amd64 go vet ./...
```

The independent engine in `internal/unlock` is tested using generated synthetic
archives, not user workbooks. CI runs tests and vet on Linux and Windows and
uploads the Windows executable. Pushing a `v*` tag runs the release workflow and
publishes the executable and SHA-256 checksum.

### Manual Windows verification checklist

Automated tests and a cross-build do **not** verify desktop interactions or Excel
compatibility. On Windows, check drag-and-drop (one file and multiple-file
refusal), Browse/cancel, protection status, responsive stage updates, Save As
defaults/cancel, overwrite confirmation, input-path refusal, actionable failures,
closing during processing, and successful opening of an output workbook in Excel
(including a macro-enabled copy without executing its macros).

## License

[MIT](LICENSE).
