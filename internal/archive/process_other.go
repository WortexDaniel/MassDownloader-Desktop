//go:build !windows

package archive

import "os/exec"

func hideWindow(c *exec.Cmd) {}
