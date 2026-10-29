package ctl

import (
	"context"
	"fmt"

	"github.com/camilbenameur/gatehaul/internal/host"
	"github.com/camilbenameur/gatehaul/internal/out"
	"github.com/camilbenameur/gatehaul/internal/runner"
)

// DownOptions configures Down.
type DownOptions struct {
	Delete bool
	JSON   bool
}

// DownRow is what Down did to one host.
type DownRow struct {
	Host   string `json:"host"`
	Action string `json:"action"` // stopped, deleted, already_stopped, skipped, failed
	Reason string `json:"reason,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

// Down stops (or deletes) the hosts whose queue is empty and that run nothing.
// A stopped host is not probed when it is only to be stopped (probing would
// wake it); to be deleted, it is woken and probed like a running one, because
// jobs that never started wait in its queue.
func (c *Controller) Down(ctx context.Context, o DownOptions) int {
	hosts, e := c.discover(ctx)
	if e != nil {
		return c.fail(o.JSON, e)
	}
	rows := []DownRow{}
	exit := out.ExitOK
	for _, h := range hosts {
		row := DownRow{Host: h.DisplayName}
		switch h.State {
		case host.Stopped:
			if !o.Delete {
				row.Action = "already_stopped"
				break
			}
			fmt.Fprintf(c.Stderr, "waking %s to check its queue before deleting it…\n", h.DisplayName)
			row = c.downIfIdle(ctx, h, true)
		case host.Running:
			row = c.downIfIdle(ctx, h, o.Delete)
		case host.Busy:
			row.Action, row.Reason = "skipped", fmt.Sprintf("it is %s: try again once it settles", h.RawState)
		case host.Failed:
			// Nothing can run there: it only costs quota until it is deleted.
			if o.Delete {
				row = c.downAct(ctx, h, true)
			} else {
				e := unusable(h)
				row.Action, row.Reason, row.Fix = "skipped", e.Message, "gatehaul down --delete"
			}
		case host.Unknown:
			e := unusable(h)
			row.Action, row.Reason, row.Fix = "skipped", e.Message, e.Fix
		default:
			row.Action, row.Reason = "skipped", "it does not exist"
		}
		if row.Action == "failed" {
			exit = out.ExitInfra
		}
		rows = append(rows, row)
	}
	if o.JSON {
		if err := out.WriteJSON(c.Stdout, struct {
			Hosts []DownRow `json:"hosts"`
		}{rows}); err != nil {
			return out.ExitInfra
		}
		return exit
	}
	if len(rows) == 0 {
		fmt.Fprintln(c.Stdout, "no gatehaul-* host")
	}
	for _, r := range rows {
		line := fmt.Sprintf("%s: %s", r.Host, r.Action)
		if r.Reason != "" {
			line += ": " + r.Reason
		}
		fmt.Fprintln(c.Stdout, line)
		if r.Fix != "" {
			fmt.Fprintf(c.Stderr, "Fix: %s\n", r.Fix)
		}
	}
	return exit
}

// downIfIdle probes h and stops (or deletes) it unless it runs or queues a job.
// A host that lacks a tool is probed all the same: it can still be stopped.
func (c *Controller) downIfIdle(ctx context.Context, h host.Host, del bool) DownRow {
	row := DownRow{Host: h.DisplayName}
	p, pe := c.probe(ctx, h)
	switch reason := busyReason(p); {
	case pe != nil && pe.Code != out.CodeMissingTool:
		row.Action, row.Reason, row.Fix = "failed", pe.Message, pe.Fix
	case reason != "":
		row.Action, row.Reason = "skipped", reason
	default:
		row = c.downAct(ctx, h, del)
	}
	return row
}

func (c *Controller) downAct(ctx context.Context, h host.Host, del bool) DownRow {
	row := DownRow{Host: h.DisplayName}
	var err error
	if del {
		row.Action, err = "deleted", c.Backend.Delete(ctx, h)
	} else {
		row.Action, err = "stopped", c.Backend.Stop(ctx, h)
	}
	if err != nil {
		oe := asError(err, out.CodeGHError, "run the gh command by hand to see the error: gh codespace list --repo "+c.Repo)
		row.Action, row.Reason, row.Fix = "failed", oe.Message, oe.Fix
	}
	return row
}

// busyReason says why a host must not be stopped, or "" when it is idle.
func busyReason(p runner.Probe) string {
	switch {
	case p.Running != nil:
		return "job " + p.Running.ID + " is running"
	case p.QueueDepth > 0:
		return fmt.Sprintf("%d job(s) are queued", p.QueueDepth)
	}
	return ""
}
