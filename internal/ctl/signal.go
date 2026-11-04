package ctl

import (
	"fmt"
	"strings"

	"github.com/camilbenameur/gatehaul/internal/job"
)

// stageCommand is the runner's stage for a job whose command itself ended.
const stageCommand = "command"

// signalNote explains an exit status above 128: bash reports a command killed
// by signal N as 128+N. Jobs run with "bash -o pipefail" (docs/SPEC.md §5.2),
// so a pipeline whose reader stops early, such as "make lint | head -40",
// fails with 141 even when the command it reads from passed. A command may
// also exit with such a status on its own, hence "usually" in the text.
func signalNote(status int) string {
	if status <= 128 || status > 128+64 {
		return ""
	}
	switch sig := status - 128; sig {
	case 13:
		return `SIGPIPE: a reader such as "| head" closed the pipe early; jobs run with pipefail, so read the log with log --tail N or --grep RE instead of piping into head`
	case 9:
		return "SIGKILL: killed, often out of memory, or by a timeout inside the command"
	case 15:
		return "SIGTERM: asked to stop, for example by a timeout inside the command"
	case 2:
		return "SIGINT: interrupted"
	case 6:
		return "SIGABRT: the program aborted"
	case 11:
		return "SIGSEGV: the program crashed (segmentation fault)"
	default:
		return fmt.Sprintf("usually killed by signal %d", sig)
	}
}

// reason is the job's reason, with the signal behind an exit status above 128
// spelled out when the command itself failed.
func (r *jobResult) reason() string {
	st := r.status
	if r.state != job.Failed || st.FailureKind != job.KindUser || st.Stage != stageCommand || st.ExitCode == nil {
		return st.Reason
	}
	note := signalNote(*st.ExitCode)
	if note == "" {
		return st.Reason
	}
	status := fmt.Sprintf("status %d", *st.ExitCode)
	if strings.Contains(st.Reason, status) {
		return strings.Replace(st.Reason, status, status+" ("+note+")", 1)
	}
	if st.Reason == "" {
		return "the command exited with " + status + " (" + note + ")"
	}
	return st.Reason + " (" + status + ": " + note + ")"
}
