// Package appdir resolves the mutable application-data directory.
// Port of app/settings.py path logic (APP_DIR, legacy migration).
package appdir

import (
	"os"
	"path/filepath"
)

// BaseDir is the folder holding data/ and downloads/. In a normal checkout
// that is the project root; a frozen (Wails) build anchors on the executable
// so user data sits next to the .exe and survives restarts.
func BaseDir() string {
	exe, err := os.Executable()
	if err != nil {
		cwd, _ := os.Getwd()
		return cwd
	}
	return filepath.Dir(exe)
}

// Dir returns the mutable application-data directory. MASSDOWNLOADER_APP_DIR
// is an explicit isolation seam for tests and diagnostics; normal launches
// keep the established data/-beside-the-app layout.
func Dir() string {
	override := os.Getenv("MASSDOWNLOADER_APP_DIR")
	if override != "" {
		if abs, err := filepath.Abs(override); err == nil {
			return abs
		}
	}
	return filepath.Join(BaseDir(), "data")
}
