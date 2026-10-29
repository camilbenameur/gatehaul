package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"

	"github.com/camilbenameur/gatehaul/internal/clock"
	"github.com/camilbenameur/gatehaul/internal/ctl"
	"github.com/camilbenameur/gatehaul/internal/detect"
	"github.com/camilbenameur/gatehaul/internal/ghx"
	"github.com/camilbenameur/gatehaul/internal/host/codespaces"
	"github.com/camilbenameur/gatehaul/internal/job"
	"github.com/camilbenameur/gatehaul/internal/out"
)

// common holds the flags every controller command shares.
type common struct {
	json *bool
	repo *string
	dir  *string
}

func newFlags(name string, stderr io.Writer) (*flag.FlagSet, common) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs, common{
		json: fs.Bool("json", false, "print one JSON object"),
		repo: fs.String("repo", "", "GitHub repository as owner/repo (default: from the git remote)"),
		dir:  fs.String("C", "", "run as if gatehaul was started in this directory"),
	}
}

// parseInterspersed parses flags that may come after positional arguments.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// splitDash cuts args at the first "--".
func splitDash(args []string) (before, after []string, found bool) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:], true
		}
	}
	return args, nil, false
}

// flagExit is the exit code of a failed flag parse.
func flagExit(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return out.ExitOK
	}
	return out.ExitUsage
}

func usageFail(stderr io.Writer, asJSON bool, msg, fix string) int {
	out.WriteError(stderr, asJSON, &out.Error{Code: out.CodeUsage, Message: msg, Fix: fix})
	return out.ExitUsage
}

// newController wires the real gh, git, clock, cache and codespaces backend.
func newController(ctx context.Context, c common, stdout, stderr io.Writer) (*ctl.Controller, int) {
	r := ghx.ExecRunner{}
	ghHost := detect.GHHost(os.Getenv).Value
	repo, e := ctl.DetectRepo(ctx, r, *c.dir, *c.repo, ghHost)
	if e != nil {
		out.WriteError(stderr, *c.json, e)
		return nil, e.Exit
	}
	cacheDir, err := job.DefaultCacheDir(os.Getenv, os.UserCacheDir)
	if err != nil {
		out.WriteError(stderr, *c.json, &out.Error{Code: out.CodeCacheError, Message: err.Error(),
			Fix: "set GATEHAUL_CACHE_DIR to a writable directory", Exit: out.ExitInfra})
		return nil, out.ExitInfra
	}
	b := codespaces.New(r, repo)
	b.GHHost = ghHost
	return ctl.New(ctl.Config{
		Runner: r, Clock: clock.Real{}, Backend: b, Cache: job.NewCache(cacheDir),
		Stdout: stdout, Stderr: stderr, Stdin: os.Stdin, IsTerminal: stdinIsTerminal,
		Dir: *c.dir, Repo: repo,
	}), 0
}

func cmdRun(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs, c := newFlags("run", stderr)
	hostN := fs.Int("host", 0, "run on gatehaul-N (default: the first running host)")
	prio := fs.Int("prio", ctl.DefaultPrio, "priority 0 to 9, lower runs first")
	timeout := fs.Duration("timeout", ctl.DefaultTimeout, "kill the command after this long")
	block := fs.Duration("block", ctl.DefaultBlock, "wait at most this long, then return the job id")
	before, after, found := splitDash(args)
	pos, err := parseInterspersed(fs, before)
	if err != nil {
		return flagExit(err)
	}
	const synopsis = "gatehaul run [<ref>] [--host N] [--prio P] [--timeout D] [--block D] [--json] -- <cmd>"
	if !found || len(after) == 0 {
		return usageFail(stderr, *c.json, "no command given after --", synopsis)
	}
	if len(pos) > 1 {
		return usageFail(stderr, *c.json, "unexpected argument "+pos[1], synopsis)
	}
	if *hostN < 0 {
		return usageFail(stderr, *c.json, "--host must be a host number", synopsis)
	}
	ref := ""
	if len(pos) == 1 {
		ref = pos[0]
	}
	ctrl, code := newController(ctx, c, stdout, stderr)
	if ctrl == nil {
		return code
	}
	return ctrl.Run(ctx, ctl.RunOptions{Ref: ref, Host: *hostN, Prio: *prio, Timeout: *timeout, Block: *block, JSON: *c.json, Cmd: after})
}

func cmdWait(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs, c := newFlags("wait", stderr)
	anyJob := fs.Bool("any", false, "return when any job ended")
	allJobs := fs.Bool("all", false, "return when every job ended (default)")
	block := fs.Duration("block", ctl.DefaultBlock, "wait at most this long")
	ids, err := parseInterspersed(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if *anyJob && *allJobs {
		return usageFail(stderr, *c.json, "--any and --all exclude each other", "gatehaul wait [<id>...] [--any|--all] [--block D]")
	}
	ctrl, code := newController(ctx, c, stdout, stderr)
	if ctrl == nil {
		return code
	}
	return ctrl.Wait(ctx, ctl.WaitOptions{IDs: ids, Any: *anyJob, Block: *block, JSON: *c.json})
}

func cmdLog(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs, c := newFlags("log", stderr)
	grep := fs.String("grep", "", "keep the lines that match this extended regular expression")
	since := fs.Int64("since", 0, "start at this byte offset (the cursor of a previous call)")
	tail := fs.Int("tail", 0, "keep only the last N lines")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(pos) > 1 {
		return usageFail(stderr, *c.json, "unexpected argument "+pos[1], "gatehaul log [<id>] [--grep RE] [--since N] [--tail N] [--json]")
	}
	id := ""
	if len(pos) == 1 {
		id = pos[0]
	}
	ctrl, code := newController(ctx, c, stdout, stderr)
	if ctrl == nil {
		return code
	}
	return ctrl.Log(ctx, ctl.LogOptions{ID: id, Grep: *grep, Since: *since, Tail: *tail, JSON: *c.json})
}

func cmdUp(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs, c := newFlags("up", stderr)
	n := fs.Int("n", 1, "how many gatehaul-<i> codespaces should exist")
	machine := fs.String("machine", "", "codespace machine type (default: the smallest allowed)")
	max := fs.Int("max", ctl.DefaultMaxHosts, "never go above this many gatehaul-* codespaces")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	dry := fs.Bool("dry-run", false, "print the plan and create nothing")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return flagExit(err)
	}
	const synopsis = "gatehaul up [-n 1] [--machine M] [--max 2] [--yes] [--dry-run] [--json]"
	if len(pos) > 0 {
		return usageFail(stderr, *c.json, "unexpected argument "+pos[0], synopsis)
	}
	if *n < 1 || *max < 1 {
		return usageFail(stderr, *c.json, "-n and --max must be at least 1", synopsis)
	}
	ctrl, code := newController(ctx, c, stdout, stderr)
	if ctrl == nil {
		return code
	}
	return ctrl.Up(ctx, ctl.UpOptions{N: *n, Machine: *machine, Max: *max, Yes: *yes, DryRun: *dry, JSON: *c.json})
}

func cmdDown(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs, c := newFlags("down", stderr)
	del := fs.Bool("delete", false, "delete the codespaces instead of stopping them")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(pos) > 0 {
		return usageFail(stderr, *c.json, "unexpected argument "+pos[0], "gatehaul down [--delete] [--json]")
	}
	ctrl, code := newController(ctx, c, stdout, stderr)
	if ctrl == nil {
		return code
	}
	return ctrl.Down(ctx, ctl.DownOptions{Delete: *del, JSON: *c.json})
}
