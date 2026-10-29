package ctl

import (
	"bufio"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/camilbenameur/gatehaul/internal/host"
	"github.com/camilbenameur/gatehaul/internal/lock"
	"github.com/camilbenameur/gatehaul/internal/out"
)

// UpOptions configures Up. Machine "" lets the backend pick the smallest one.
type UpOptions struct {
	N       int
	Machine string
	Max     int
	Yes     bool
	DryRun  bool
	JSON    bool
}

// CreatedHost is a host that Up created.
type CreatedHost struct {
	DisplayName string `json:"display_name"`
	Name        string `json:"name"`
}

// UpReport is the JSON output of up.
type UpReport struct {
	DryRun      bool          `json:"dry_run"`
	Requested   int           `json:"requested"`
	Existing    []string      `json:"existing"`
	ToCreate    []string      `json:"to_create"`
	Created     []CreatedHost `json:"created"`
	IdleTimeout string        `json:"idle_timeout"`
	Retention   string        `json:"retention"`
	CostNote    string        `json:"cost_note"`
}

const lockWait = 2 * time.Minute

// Up creates the missing gatehaul-1..N hosts, under a file lock, never above
// the cap. Existing hosts are left alone: a stopped one wakes on first use.
func (c *Controller) Up(ctx context.Context, o UpOptions) int {
	if o.N < 1 {
		return c.fail(o.JSON, usageError("-n must be at least 1", "gatehaul up -n 1"))
	}
	if o.Max < 1 {
		return c.fail(o.JSON, usageError("--max must be at least 1", "gatehaul up --max 2"))
	}
	if o.N > o.Max {
		return c.fail(o.JSON, tooMany(o.N, o.N, o.Max))
	}
	hosts, e := c.discover(ctx)
	if e != nil {
		return c.fail(o.JSON, e)
	}
	missing, e := planUp(hosts, o)
	if e != nil {
		return c.fail(o.JSON, e)
	}
	rep := UpReport{
		DryRun: o.DryRun, Requested: o.N, Existing: existingNames(hosts), ToCreate: displayNames(missing),
		Created: []CreatedHost{}, IdleTimeout: DefaultIdleTimeout.String(), Retention: DefaultRetention.String(), CostNote: CostNote,
	}
	if len(missing) == 0 {
		return c.upDone(rep, o.JSON, "nothing to create: "+strings.Join(displayNames(upper(o.N)), ", ")+" already exist")
	}
	if !o.JSON {
		machine := o.Machine
		if machine == "" {
			machine = "the smallest allowed"
		}
		fmt.Fprintf(c.Stdout, "will create: %s (machine: %s, idle timeout %s, retention %s)\n",
			strings.Join(rep.ToCreate, ", "), machine, DefaultIdleTimeout, DefaultRetention)
		fmt.Fprintf(c.Stdout, "cost: %s\n", CostNote)
	}
	if o.DryRun {
		return c.upDone(rep, o.JSON, "dry run: nothing was created")
	}
	if !o.Yes {
		fix := fmt.Sprintf("gatehaul up -n %d --yes", o.N)
		if o.Machine != "" {
			fix += " --machine " + o.Machine
		}
		if o.Max != DefaultMaxHosts {
			fix += fmt.Sprintf(" --max %d", o.Max)
		}
		if !c.IsTerminal() {
			return c.fail(o.JSON, &out.Error{Code: out.CodeConfirmationNeeded,
				Message: "creating codespaces uses your quota and needs a confirmation, but there is no terminal to ask on",
				Fix:     fix, Exit: out.ExitInfra})
		}
		fmt.Fprintf(c.Stderr, "Create %d codespace(s)? [y/N] ", len(missing))
		line, _ := bufio.NewReader(c.Stdin).ReadString('\n')
		a := strings.ToLower(strings.TrimSpace(line))
		if a == "" {
			// End of input or an empty answer is not a "no" that a person
			// typed: nothing was confirmed, and that is not a success.
			fmt.Fprintln(c.Stderr)
			return c.fail(o.JSON, &out.Error{Code: out.CodeConfirmationNeeded,
				Message: "no answer was given to the confirmation prompt, so nothing was created",
				Fix:     fix, Exit: out.ExitInfra})
		}
		if a != "y" && a != "yes" {
			fmt.Fprintln(c.Stderr, "not confirmed")
			return c.upDone(rep, o.JSON, "nothing was created")
		}
	}

	lctx, cancel := context.WithTimeout(ctx, lockWait)
	defer cancel()
	release, err := lock.Acquire(lctx, filepath.Join(c.Cache.Dir(), "up.lock"))
	if err != nil {
		if ctx.Err() != nil {
			return c.fail(o.JSON, interrupted())
		}
		return c.fail(o.JSON, &out.Error{Code: out.CodeLockBusy, Message: "another gatehaul up holds the lock: " + err.Error(),
			Fix: fmt.Sprintf("wait for it to finish, then run: gatehaul up -n %d --yes", o.N), Exit: out.ExitInfra})
	}
	defer func() { _ = release() }()

	// Someone else may have created hosts while we waited: list again.
	if inv, ok := c.Backend.(interface{ Invalidate() }); ok {
		inv.Invalidate()
	}
	if hosts, e = c.discover(ctx); e != nil {
		return c.fail(o.JSON, e)
	}
	if missing, e = planUp(hosts, o); e != nil {
		return c.fail(o.JSON, e)
	}
	rep.Existing, rep.ToCreate = existingNames(hosts), displayNames(missing)
	for _, n := range missing {
		h, err := c.Backend.Create(ctx, host.CreateOptions{
			Num: n, Repo: c.Repo, Machine: o.Machine, IdleTimeout: DefaultIdleTimeout, Retention: DefaultRetention,
		})
		if err != nil {
			return c.fail(o.JSON, asError(err, out.CodeCreateFailed, "check what exists with gh codespace list --repo "+c.Repo+", then run gatehaul up again"))
		}
		rep.Created = append(rep.Created, CreatedHost{DisplayName: h.DisplayName, Name: h.Name})
		if !o.JSON {
			fmt.Fprintf(c.Stdout, "created %s (%s)\n", h.DisplayName, h.Name)
		}
	}
	return c.upDone(rep, o.JSON, "")
}

// upDone prints the JSON report, or the closing line for humans.
func (c *Controller) upDone(rep UpReport, asJSON bool, msg string) int {
	if rep.Existing == nil {
		rep.Existing = []string{}
	}
	if rep.ToCreate == nil {
		rep.ToCreate = []string{}
	}
	if rep.Created == nil {
		rep.Created = []CreatedHost{}
	}
	if asJSON {
		if err := out.WriteJSON(c.Stdout, rep); err != nil {
			return out.ExitInfra
		}
		return out.ExitOK
	}
	if msg != "" {
		fmt.Fprintln(c.Stdout, msg)
	}
	return out.ExitOK
}

// planUp lists the numbers 1..N that have no host yet. A host that is listed
// occupies its number whatever its state: a codespace that is still being
// deleted is not recreated while the list shows it (docs/SPEC.md §4), and one
// that cannot be used is reported instead of being silently counted.
func planUp(hosts []host.Host, o UpOptions) ([]int, *out.Error) {
	have := map[int]host.Host{}
	for _, h := range hosts {
		have[h.Num] = h
	}
	var missing []int
	for i := 1; i <= o.N; i++ {
		h, ok := have[i]
		switch {
		case !ok:
			missing = append(missing, i)
		case unusable(h) != nil:
			return nil, unusable(h)
		case h.State == host.NotFound:
			return nil, &out.Error{Code: out.CodeHostState,
				Message: fmt.Sprintf("%s is still listed as %q: it is being deleted", h.DisplayName, h.RawState),
				Fix:     "wait a minute until gh codespace list no longer shows it, then run gatehaul up again", Exit: out.ExitInfra}
		}
	}
	if total := len(have) + len(missing); len(missing) > 0 && total > o.Max {
		return nil, tooMany(o.N, total, o.Max)
	}
	return missing, nil
}

func tooMany(n, total, max int) *out.Error {
	return &out.Error{Code: out.CodeTooManyHosts,
		Message: fmt.Sprintf("that would make %d gatehaul-* codespaces, above the limit of %d", total, max),
		Fix:     fmt.Sprintf("raise the limit if you accept the quota use: gatehaul up -n %d --max %d, or remove one: gatehaul down --delete", n, total),
		Exit:    out.ExitInfra}
}

func upper(n int) []int {
	s := make([]int, n)
	for i := range s {
		s[i] = i + 1
	}
	return s
}

func displayNames(nums []int) []string {
	s := make([]string, len(nums))
	for i, n := range nums {
		s[i] = host.DisplayName(n)
	}
	return s
}

func existingNames(hosts []host.Host) []string {
	var s []string
	for _, h := range hosts {
		if h.State != host.NotFound {
			s = append(s, h.DisplayName)
		}
	}
	return s
}
