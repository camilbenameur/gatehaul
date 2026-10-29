// Command gatehaul offloads a coding agent's builds and tests to remote
// sandboxes and refuses green verdicts without proof that the tests ran.
//
// This release implements "doctor" and "version"; the other commands are on
// the roadmap in docs/SPEC.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"

	"github.com/camilbenameur/gatehaul/internal/doctor"
	"github.com/camilbenameur/gatehaul/internal/out"
)

// version is set at release time with -ldflags "-X main.version=v0.1.0".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// planned lists the commands of v0.1 that are not implemented yet.
var planned = map[string]bool{
	"up": true, "down": true, "run": true, "gen": true, "wait": true, "log": true,
	"status": true, "queue": true, "cancel": true, "gate": true, "hook": true,
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return out.ExitUsage
	}
	switch cmd := args[0]; {
	case cmd == "doctor":
		return cmdDoctor(ctx, args[1:], stdout, stderr)
	case cmd == "version" || cmd == "--version":
		return cmdVersion(args[1:], stdout, stderr)
	case cmd == "help" || cmd == "-h" || cmd == "--help":
		usage(stdout)
		return out.ExitOK
	case planned[cmd]:
		out.WriteError(stderr, false, &out.Error{
			Code:    out.CodeUsage,
			Message: "gatehaul " + cmd + " is not implemented yet",
			Fix:     "see the roadmap in docs/SPEC.md; gatehaul doctor works today",
		})
		return out.ExitUsage
	default:
		out.WriteError(stderr, false, &out.Error{
			Code:    out.CodeUsage,
			Message: fmt.Sprintf("unknown command %q", cmd),
			Fix:     "gatehaul help",
		})
		return out.ExitUsage
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `gatehaul: run your coding agent's builds and tests in remote sandboxes,
and refuse green verdicts without proof that the tests ran.

Usage:
  gatehaul doctor [--json] [--repo owner/repo] [-C dir]
                     check gh, authentication, git, the repository and the hosts;
                     every problem comes with the command that fixes it
  gatehaul version [--json]

Planned for v0.1: up, down, run, gen, wait, log, status, queue, cancel, gate, hook.

Exit codes: 0 ok, 1 failed, 2 usage, 3 still running, 4 false green, 5 environment, 6 patch refused.
`)
}

func cmdDoctor(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the report as JSON")
	repo := fs.String("repo", "", "GitHub repository as owner/repo (default: from the git remote)")
	dir := fs.String("C", "", "run as if gatehaul was started in this directory")
	if err := fs.Parse(args); err != nil {
		return out.ExitUsage
	}
	if fs.NArg() > 0 {
		out.WriteError(stderr, *asJSON, &out.Error{Code: out.CodeUsage, Message: "unexpected argument " + fs.Arg(0), Fix: "gatehaul doctor [--json] [--repo owner/repo] [-C dir]"})
		return out.ExitUsage
	}

	report := doctor.Run(ctx, doctor.Options{Dir: *dir, Repo: *repo})
	if *asJSON {
		if err := out.WriteJSON(stdout, report); err != nil {
			return out.ExitInfra
		}
	} else {
		doctor.Render(stdout, report)
	}
	if !report.OK {
		return out.ExitInfra
	}
	return out.ExitOK
}

func cmdVersion(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print as JSON")
	if err := fs.Parse(args); err != nil {
		return out.ExitUsage
	}
	v := currentVersion()
	if *asJSON {
		_ = out.WriteJSON(stdout, map[string]string{
			"version": v, "go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH,
		})
		return out.ExitOK
	}
	fmt.Fprintf(stdout, "gatehaul %s (%s, %s/%s)\n", v, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	return out.ExitOK
}

// currentVersion prefers the release version, then the module version that
// "go install ...@vX" records, then "dev".
func currentVersion() string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return version
}
