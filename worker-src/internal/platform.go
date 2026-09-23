package internal

import (
	"os"
	"runtime"
)

func goos() string   { return runtime.GOOS }
func goarch() string { return runtime.GOARCH }

// homeDir is the worker user's home (where `~` resolves), or "" when the
// platform has none.
func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}
