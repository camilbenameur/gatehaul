package detect

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// NextEnv names the variable that overrides how gatehaul names itself in the
// commands it prints (next: lines), for a wrapper script that adds a prelude:
// GATEHAUL_NEXT="bash scripts/gatehaul.sh".
const NextEnv = "GATEHAUL_NEXT"

// Where the prefix of printed commands came from.
const (
	SelfFromEnv        = "env"        // $GATEHAUL_NEXT
	SelfFromPath       = "path"       // "gatehaul" on PATH is this binary
	SelfFromExecutable = "executable" // not on PATH (or another one is): the full path
	SelfUnknown        = "unknown"    // the running binary could not be located
)

// Self is how the commands gatehaul prints name gatehaul, so that they can be
// run as written (docs/SPEC.md §3).
type Self struct {
	Prefix string `json:"prefix"`
	Source string `json:"source"`
	Exe    string `json:"exe,omitempty"`     // the running binary, when known
	OnPath string `json:"on_path,omitempty"` // what "gatehaul" resolves to on PATH, when it does
}

// Invocation is what DetectSelf reads from the process. Zero fields use the
// real environment.
type Invocation struct {
	Getenv     func(string) string
	Executable func() (string, error)
	LookPath   func(string) (string, error)
	SameFile   func(a, b string) bool
}

// DetectSelf returns the prefix of the commands gatehaul prints:
// $GATEHAUL_NEXT when set; "gatehaul" when the gatehaul found on PATH is the
// running binary; otherwise the running binary's absolute path, with forward
// slashes (which bash, PowerShell and cmd all accept) and shell-quoted when it
// needs to be. A bare "gatehaul" may not be on PATH (some agent sessions lack
// ~/.local/bin) or may be another version.
func DetectSelf(inv Invocation) Self {
	if inv.Getenv == nil {
		inv.Getenv = os.Getenv
	}
	if inv.Executable == nil {
		inv.Executable = os.Executable
	}
	if inv.LookPath == nil {
		inv.LookPath = exec.LookPath
	}
	if inv.SameFile == nil {
		inv.SameFile = sameFile
	}
	if v := strings.TrimSpace(inv.Getenv(NextEnv)); v != "" && printable(v) {
		return Self{Prefix: v, Source: SelfFromEnv}
	}
	// os.Executable returns an absolute path.
	exe, err := inv.Executable()
	if err != nil || exe == "" {
		return Self{Prefix: "gatehaul", Source: SelfUnknown}
	}
	s := Self{Exe: exe}
	if p, err := inv.LookPath("gatehaul"); err == nil && p != "" {
		s.OnPath = p
		if inv.SameFile(p, exe) {
			s.Prefix, s.Source = "gatehaul", SelfFromPath
			return s
		}
	}
	s.Prefix, s.Source = quotePath(filepath.ToSlash(exe)), SelfFromExecutable
	return s
}

func sameFile(a, b string) bool {
	sa, err := os.Stat(a)
	if err != nil {
		return false
	}
	sb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(sa, sb)
}

// printable refuses control characters: the prefix ends up on one line.
func printable(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

var plainPath = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./~-]+$`)

// quotePath single-quotes p for a POSIX shell when it holds anything but
// plain path characters.
func quotePath(p string) string {
	if plainPath.MatchString(p) {
		return p
	}
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}
