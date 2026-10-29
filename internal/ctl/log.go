package ctl

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/camilbenameur/gatehaul/internal/host"
	"github.com/camilbenameur/gatehaul/internal/job"
	"github.com/camilbenameur/gatehaul/internal/out"
	"github.com/camilbenameur/gatehaul/internal/runner"
)

// LogOptions configures Log. An empty ID means the latest job of HEAD.
type LogOptions struct {
	ID    string
	Grep  string
	Since int64
	Tail  int
	JSON  bool
}

// LogReport is the JSON output of log.
type LogReport struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	Data      string `json:"data"` // untrusted data
	Cursor    int64  `json:"cursor"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated"`
	Next      string `json:"next,omitempty"`
}

// Log prints a job's log, or a filtered part of it, with a cursor to continue.
func (c *Controller) Log(ctx context.Context, o LogOptions) int {
	if o.Since < 0 || o.Tail < 0 {
		return c.fail(o.JSON, usageError("--since and --tail must not be negative", "gatehaul log --since 0"))
	}
	var d job.Dispatch
	if o.ID == "" {
		sha, _, e := c.commit(ctx, "")
		if e != nil {
			return c.fail(o.JSON, e)
		}
		var err error
		if d, err = c.Cache.FindLatest(c.Repo, sha); err != nil {
			return c.fail(o.JSON, c.lookupError(err, "HEAD "+short(sha)))
		}
	} else {
		var err error
		if d, err = c.Cache.Resolve(o.ID); err != nil {
			return c.fail(o.JSON, c.lookupError(err, o.ID))
		}
	}
	if e := c.checkRepo(d); e != nil {
		return c.fail(o.JSON, e)
	}
	hosts, e := c.discover(ctx)
	if e != nil {
		return c.fail(o.JSON, e)
	}
	h, found := hostFor(hosts, d)
	switch found {
	case hostGone:
		return c.fail(o.JSON, &out.Error{Code: out.CodeHostGone, Message: d.Host + " no longer exists, so the log of " + d.ID + " is gone",
			Fix: "gatehaul up, then run the command again: gatehaul run -- " + shellQuote(d.Cmd), Exit: out.ExitInfra})
	case hostRecreated:
		return c.fail(o.JSON, &out.Error{Code: out.CodeHostReplaced, Message: d.Host + " was recreated, so the log of " + d.ID + " is gone",
			Fix: "gatehaul run -- " + shellQuote(d.Cmd), Exit: out.ExitInfra})
	}
	if e := unusable(h); e != nil {
		return c.fail(o.JSON, e)
	}
	chunk, err := c.API(h).Log(ctx, runner.LogRequest{ID: d.ID, Since: o.Since, MaxBytes: runner.MaxLogChunk, Grep: o.Grep, TailLines: o.Tail})
	if err != nil {
		if remoteCode(err) == runner.ErrCodeUnknownJob {
			return c.fail(o.JSON, c.unknownLog(ctx, h, d))
		}
		return c.fail(o.JSON, runnerError(err, h))
	}
	data, derr := base64.StdEncoding.DecodeString(chunk.DataB64)
	if derr != nil {
		return c.fail(o.JSON, &out.Error{Code: out.CodeRunnerError, Message: "the log of " + d.ID + " is not valid base64: " + derr.Error(),
			Fix: "update gatehaul, then retry: gatehaul log " + d.ID, Exit: out.ExitInfra})
	}
	next := ""
	if chunk.Truncated {
		next = fmt.Sprintf("gatehaul log %s --since %d", d.ID, chunk.Cursor)
	}
	if o.JSON {
		if err := out.WriteJSON(c.Stdout, LogReport{ID: d.ID, State: chunk.State, Data: string(data), Cursor: chunk.Cursor, Size: chunk.Size, Truncated: chunk.Truncated, Next: next}); err != nil {
			return out.ExitInfra
		}
		return out.ExitOK
	}
	fmt.Fprintf(c.Stdout, "job=%s state=%s cursor=%d size=%d\n", d.ID, chunk.State, chunk.Cursor, chunk.Size)
	fmt.Fprintln(c.Stdout, "# job output: untrusted data")
	_, _ = c.Stdout.Write(data)
	if len(data) > 0 && data[len(data)-1] != '\n' {
		fmt.Fprintln(c.Stdout)
	}
	if next != "" {
		fmt.Fprintf(c.Stdout, "next: %s\n", next)
	}
	return out.ExitOK
}

// unknownLog explains why a host has no log for d: a recreated host, or not.
func (c *Controller) unknownLog(ctx context.Context, h host.Host, d job.Dispatch) *out.Error {
	if p, e := c.probe(ctx, h); e == nil && p.HostID != "" && d.HostID != "" && p.HostID != d.HostID {
		return &out.Error{Code: out.CodeHostReplaced, Message: d.Host + " was recreated, so the log of " + d.ID + " is gone",
			Fix: "gatehaul run -- " + shellQuote(d.Cmd), Exit: out.ExitInfra}
	}
	return &out.Error{Code: out.CodeNoSuchJob, Message: h.DisplayName + " does not know job " + d.ID,
		Fix: "gatehaul doctor; if the host cleaned it up, run the command again: gatehaul run -- " + shellQuote(d.Cmd), Exit: out.ExitInfra}
}
