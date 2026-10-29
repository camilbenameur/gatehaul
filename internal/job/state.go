// Package job holds job ids, states, and the local cache of dispatched jobs.
package job

import (
	"fmt"

	"github.com/camilbenameur/gatehaul/internal/out"
)

// State is the state of a job. Terminal states are never rewritten.
type State string

const (
	Queued      State = "queued"
	Running     State = "running"
	Succeeded   State = "succeeded"
	Failed      State = "failed"
	TimedOut    State = "timed_out"
	Cancelled   State = "cancelled"
	Lost        State = "lost"
	InfraFailed State = "infra_failed"
)

// Failure kinds, set on failed, timed_out, lost and infra_failed jobs.
const (
	KindUser  = "user"
	KindInfra = "infra"
	KindLost  = "lost"
	KindGate  = "gate"
)

// info is the one truth table of states (docs/SPEC.md §3).
var info = map[State]struct {
	terminal bool
	exit     int
}{
	Queued:      {false, out.ExitRunning},
	Running:     {false, out.ExitRunning},
	Succeeded:   {true, out.ExitOK},
	Failed:      {true, out.ExitRed},
	TimedOut:    {true, out.ExitRed},
	Cancelled:   {true, out.ExitRed},
	Lost:        {true, out.ExitInfra},
	InfraFailed: {true, out.ExitInfra},
}

// Parse returns the State named s. An unknown state is an error, never a
// default.
func Parse(s string) (State, error) {
	st := State(s)
	if _, ok := info[st]; !ok {
		return "", fmt.Errorf("unknown job state %q", s)
	}
	return st, nil
}

// Terminal reports whether s is final.
func (s State) Terminal() bool { return info[s].terminal }

// ExitCode is the exit code of run, gen, wait and gate for a job in state s.
// Commands that only read (status, log, queue) exit 0 instead.
func (s State) ExitCode() int {
	i, ok := info[s]
	if !ok {
		return out.ExitInfra
	}
	return i.exit
}
