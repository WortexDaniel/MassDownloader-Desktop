//go:build windows

package main

import (
	"os/exec"
)

// This is a GUI executable: it has no console window to hide. STARTF_USESHOWWINDOW
// with SW_HIDE also hides the first Wails window, leaving an invisible feed.
func configureFeedProcess(cmd *exec.Cmd) { cmd.SysProcAttr = nil }
