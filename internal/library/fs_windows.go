//go:build windows

package library

import "golang.org/x/sys/windows"

// MOVEFILE_REPLACE_EXISTING is deliberately absent.
func publishExclusive(from, to string) error {
	src, e := windows.UTF16PtrFromString(from)
	if e != nil {
		return e
	}
	dst, e := windows.UTF16PtrFromString(to)
	if e != nil {
		return e
	}
	return windows.MoveFileEx(src, dst, windows.MOVEFILE_WRITE_THROUGH)
}
