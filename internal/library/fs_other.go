//go:build !windows

package library

import "os"

func publishExclusive(from, to string) error {
	if err := os.Link(from, to); err != nil {
		return err
	}
	return os.Remove(from)
}
