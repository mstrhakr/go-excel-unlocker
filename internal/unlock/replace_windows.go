//go:build windows

package unlock

import (
	"syscall"
	"unsafe"
)

var moveFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")

func replaceFile(source, destination string) error {
	const replaceExisting = 0x1
	return moveFile(source, destination, replaceExisting)
}

func publishNewFile(source, destination string) error {
	// Without REPLACE_EXISTING, MoveFileEx fails if a racing destination
	// appears. Unlike hard links, this also works on FAT and exFAT.
	return moveFile(source, destination, 0)
}

func moveFile(source, destination string, flags uintptr) error {
	from, err := syscall.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := syscall.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	// Same-directory MoveFileEx replaces the directory entry atomically.
	// COPY_ALLOWED is deliberately absent: never fall back to a partial copy.
	const writeThrough = 0x8
	ok, _, callErr := moveFileEx.Call(uintptr(unsafe.Pointer(from)), uintptr(unsafe.Pointer(to)), flags|writeThrough)
	if ok == 0 {
		return callErr
	}
	return nil
}
