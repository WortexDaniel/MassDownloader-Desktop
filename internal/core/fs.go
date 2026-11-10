package core

import "os"

// createFile creates/overwrites dest (the .part file), mirroring Python's
// open(dest, "wb").
func createFile(dest string) (*os.File, error) {
	return os.Create(dest)
}
