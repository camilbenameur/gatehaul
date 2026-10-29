//go:build !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd && !windows

package main

// stdinIsTerminal is false where gatehaul has no terminal check: "gatehaul up"
// then asks for --yes instead of a prompt.
func stdinIsTerminal() bool { return false }
