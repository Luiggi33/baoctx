//go:build windows

package main

// permWarning is a no-op on Windows (per-user %AppData% ACLs).
func permWarning(string) string { return "" }
