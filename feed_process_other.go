//go:build !windows

package main

import "os/exec"

func configureFeedProcess(cmd *exec.Cmd) {}
