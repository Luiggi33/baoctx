//go:build !windows

package main

import "os"

// permWarning returns a non-empty message if the file is accessible by group/other.
func permWarning(path string) string {
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm()&0o077 == 0 {
		return ""
	}
	return path + " is accessible by group/other (mode " + fi.Mode().Perm().String() + "); run chmod 600"
}
