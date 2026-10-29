//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// stdinIsTerminal reports whether a person can answer a prompt: stdin must be
// a console, not NUL (a character device too) or a pipe. A terminal that gives
// the process a pipe (Git Bash) needs --yes.
func stdinIsTerminal() bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(os.Stdin.Fd()), &mode) == nil
}
