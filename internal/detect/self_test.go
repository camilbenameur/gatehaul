package detect

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func fakeInvocation(env map[string]string, exe, onPath string) Invocation {
	return Invocation{
		Getenv:     func(k string) string { return env[k] },
		Executable: func() (string, error) { return exe, nil },
		LookPath: func(name string) (string, error) {
			if name != "gatehaul" {
				return "", errors.New("unexpected lookup of " + name)
			}
			if onPath == "" {
				return "", exec.ErrNotFound
			}
			return onPath, nil
		},
		SameFile: func(a, b string) bool { return a == b },
	}
}

func TestDetectSelf(t *testing.T) {
	cases := []struct {
		name        string
		env         map[string]string
		exe, onPath string
		want        Self
	}{
		{"on PATH", nil, "/opt/bin/gatehaul", "/opt/bin/gatehaul",
			Self{Prefix: "gatehaul", Source: SelfFromPath, Exe: "/opt/bin/gatehaul", OnPath: "/opt/bin/gatehaul"}},
		{"not on PATH", nil, "/home/me/.local/bin/gatehaul", "",
			Self{Prefix: "/home/me/.local/bin/gatehaul", Source: SelfFromExecutable, Exe: "/home/me/.local/bin/gatehaul"}},
		{"another gatehaul on PATH", nil, "/home/me/.local/bin/gatehaul", "/usr/bin/gatehaul",
			Self{Prefix: "/home/me/.local/bin/gatehaul", Source: SelfFromExecutable, Exe: "/home/me/.local/bin/gatehaul", OnPath: "/usr/bin/gatehaul"}},
		{"a path that needs quoting", nil, "/home/me/My Tools/gatehaul", "",
			Self{Prefix: "'/home/me/My Tools/gatehaul'", Source: SelfFromExecutable, Exe: "/home/me/My Tools/gatehaul"}},
		{"override", map[string]string{NextEnv: "  bash scripts/gatehaul.sh "}, "/opt/bin/gatehaul", "/opt/bin/gatehaul",
			Self{Prefix: "bash scripts/gatehaul.sh", Source: SelfFromEnv}},
		{"override with a newline is ignored", map[string]string{NextEnv: "bash x.sh\nrm -rf /"}, "/opt/bin/gatehaul", "/opt/bin/gatehaul",
			Self{Prefix: "gatehaul", Source: SelfFromPath, Exe: "/opt/bin/gatehaul", OnPath: "/opt/bin/gatehaul"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DetectSelf(fakeInvocation(c.env, c.exe, c.onPath)); got != c.want {
				t.Errorf("DetectSelf = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestDetectSelfWithoutExecutable(t *testing.T) {
	inv := fakeInvocation(nil, "", "")
	inv.Executable = func() (string, error) { return "", errors.New("no /proc") }
	if got := DetectSelf(inv); got.Prefix != "gatehaul" || got.Source != SelfUnknown {
		t.Errorf("DetectSelf = %+v", got)
	}
}

// On Windows the full path uses forward slashes, which bash, PowerShell and
// cmd all accept, and needs no quoting.
func TestDetectSelfWindowsPath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("backslashes are path separators on Windows only")
	}
	got := DetectSelf(fakeInvocation(nil, `C:\Users\me\.local\bin\gatehaul.exe`, ""))
	if got.Prefix != "C:/Users/me/.local/bin/gatehaul.exe" {
		t.Errorf("prefix = %q", got.Prefix)
	}
}

// The real lookup: a binary found on PATH under the name gatehaul is the one
// running, so the prefix is the bare name.
func TestDetectSelfRealPath(t *testing.T) {
	dir := t.TempDir()
	name := "gatehaul"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	exe := filepath.Join(dir, name)
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	got := DetectSelf(Invocation{
		Getenv:     func(string) string { return "" },
		Executable: func() (string, error) { return exe, nil },
	})
	if got.Prefix != "gatehaul" || got.Source != SelfFromPath {
		t.Errorf("DetectSelf = %+v, want the bare name", got)
	}
	t.Setenv("PATH", t.TempDir())
	got = DetectSelf(Invocation{
		Getenv:     func(string) string { return "" },
		Executable: func() (string, error) { return exe, nil },
	})
	if got.Source != SelfFromExecutable || got.Prefix != quotePath(filepath.ToSlash(exe)) {
		t.Errorf("DetectSelf = %+v, want the full path", got)
	}
}
