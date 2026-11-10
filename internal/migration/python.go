// Package migration imports Python data into a fresh Go install. Source
// databases are opened read-only; VACUUM INTO takes a consistent WAL snapshot.
package migration

import (
	"database/sql"
	"fmt"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"strings"
)

func CopyDatabase(source, dest string) error {
	if _, err := os.Stat(dest); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Stat(source); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(dest), ".python-import-*.db")
	if err != nil {
		return err
	}
	stage := temp.Name()
	temp.Close()
	defer os.Remove(stage)
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(source)+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(`VACUUM INTO ?`, stage); err != nil {
		return fmt.Errorf("snapshot %s: %w", source, err)
	}
	if _, err := os.Stat(dest); err == nil {
		return nil
	}
	return os.Rename(stage, dest)
}
func Import(sourceRoot, dataDir string) error {
	source := filepath.Join(sourceRoot, "data")
	for _, name := range []string{"app.db", "history.db"} {
		path := filepath.Join(source, name)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		}
		if err := CopyDatabase(path, filepath.Join(dataDir, name)); err != nil {
			return err
		}
	}
	for _, name := range []string{"favorites.json"} {
		src, dest := filepath.Join(source, name), filepath.Join(dataDir, name)
		if _, err := os.Stat(dest); err == nil {
			continue
		}
		if data, err := os.ReadFile(src); err == nil {
			if err := os.WriteFile(dest, data, 0600); err != nil {
				return err
			}
		}
	}
	return nil
}
func DetectPythonRoot(base string) string {
	for n := 0; n < 4; n++ {
		if st, err := os.Stat(filepath.Join(base, "main.py")); err == nil && !st.IsDir() {
			if st, err := os.Stat(filepath.Join(base, "app", "core")); err == nil && st.IsDir() {
				return base
			}
		}
		parent := filepath.Dir(base)
		if strings.EqualFold(parent, base) {
			break
		}
		base = parent
	}
	return ""
}
