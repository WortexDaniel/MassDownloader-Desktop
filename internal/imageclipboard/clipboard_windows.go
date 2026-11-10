//go:build windows

package imageclipboard

import (
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	clipboardMu             sync.Mutex
	user32                  = windows.NewLazySystemDLL("user32.dll")
	kernel32                = windows.NewLazySystemDLL("kernel32.dll")
	createWindow            = user32.NewProc("CreateWindowExW")
	destroyWindow           = user32.NewProc("DestroyWindow")
	openClipboard           = user32.NewProc("OpenClipboard")
	closeClipboard          = user32.NewProc("CloseClipboard")
	emptyClipboard          = user32.NewProc("EmptyClipboard")
	setClipboardData        = user32.NewProc("SetClipboardData")
	registerClipboardFormat = user32.NewProc("RegisterClipboardFormatW")
	globalAlloc             = kernel32.NewProc("GlobalAlloc")
	globalLock              = kernel32.NewProc("GlobalLock")
	globalUnlock            = kernel32.NewProc("GlobalUnlock")
	globalFree              = kernel32.NewProc("GlobalFree")
	moveMemory              = kernel32.NewProc("RtlMoveMemory")
)

func callError(operation string, err error) error {
	if err != syscall.Errno(0) {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return fmt.Errorf("%s failed", operation)
}

func allocate(data []byte) (uintptr, error) {
	handle, _, err := globalAlloc.Call(2, uintptr(len(data))) // GMEM_MOVEABLE
	if handle == 0 {
		return 0, callError("GlobalAlloc", err)
	}
	ptr, _, err := globalLock.Call(handle)
	if ptr == 0 {
		globalFree.Call(handle)
		return 0, callError("GlobalLock", err)
	}
	moveMemory.Call(ptr, uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)))
	runtime.KeepAlive(data)
	globalUnlock.Call(handle)
	return handle, nil
}

func SetPNG(data []byte) error {
	v5, legacy, err := clipboardDIBs(data)
	if err != nil {
		return err
	}
	clipboardMu.Lock()
	defer clipboardMu.Unlock()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// OpenClipboard needs a non-null owner before EmptyClipboard/SetClipboardData.
	// A message-only built-in STATIC window never creates a visible UI window.
	class, _ := windows.UTF16PtrFromString("STATIC")
	owner, _, err := createWindow.Call(0, uintptr(unsafe.Pointer(class)), 0, 0, 0, 0, 0, 0, ^uintptr(2), 0, 0, 0)
	if owner == 0 {
		return callError("CreateWindowExW", err)
	}
	defer destroyWindow.Call(owner)
	name, _ := windows.UTF16PtrFromString("PNG")
	pngFormat, _, err := registerClipboardFormat.Call(uintptr(unsafe.Pointer(name)))
	if pngFormat == 0 {
		return callError("RegisterClipboardFormatW", err)
	}
	formats := []uintptr{17, 8, pngFormat} // CF_DIBV5, CF_DIB, registered PNG
	handles := make([]uintptr, len(formats))
	defer func() {
		for _, handle := range handles {
			if handle != 0 {
				globalFree.Call(handle)
			}
		}
	}()
	for i, bytes := range [][]byte{v5, legacy, data} {
		if handles[i], err = allocate(bytes); err != nil {
			return err
		}
	}
	var opened uintptr
	for attempt := 0; attempt < 20; attempt++ {
		opened, _, err = openClipboard.Call(owner)
		if opened != 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if opened == 0 {
		return callError("OpenClipboard", err)
	}
	defer closeClipboard.Call()
	if result, _, err := emptyClipboard.Call(); result == 0 {
		return callError("EmptyClipboard", err)
	}
	for i, format := range formats {
		if result, _, err := setClipboardData.Call(format, handles[i]); result == 0 {
			return callError("SetClipboardData", err)
		}
		handles[i] = 0 // Windows owns successfully submitted memory.
	}
	return nil
}
