//go:build !windows

package unlock

import "os"

func replaceFile(source, destination string) error {
	return os.Rename(source, destination)
}

func publishNewFile(source, destination string) error {
	// Link atomically creates the destination only if absent. The temporary
	// source is in the same directory and is removed by the caller afterward.
	return os.Link(source, destination)
}
