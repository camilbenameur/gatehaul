// Package out defines what gatehaul prints: exit codes, stable error codes and
// diagnostic checks. Every problem carries a Fix, the command or action that
// resolves it, because the reader is often an agent that cannot ask.
package out

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Exit codes shared by every command (docs/SPEC.md §3).
const (
	ExitOK           = 0 // success, or green verdict
	ExitRed          = 1 // the job ran and failed
	ExitUsage        = 2 // bad invocation; also the Claude Code Stop hook's "block" code
	ExitRunning      = 3 // still running: call wait with the returned id
	ExitFalseGreen   = 4 // the gate cannot prove the tests ran
	ExitInfra        = 5 // infrastructure or environment problem, see Fix
	ExitPatchRefused = 6 // the patch from a gen job was refused
)

// Code is a stable, machine-readable error code. Codes never change meaning
// once released; add new ones instead.
type Code string

const (
	CodeUsage          Code = "usage"
	CodeGHMissing      Code = "gh_missing"
	CodeGHTooOld       Code = "gh_too_old"
	CodeGHError        Code = "gh_error"
	CodeNotLoggedIn    Code = "not_logged_in"
	CodeScopeMissing   Code = "scope_missing"
	CodeRateLimited    Code = "rate_limited"
	CodeNetwork        Code = "network"
	CodeGitMissing     Code = "git_missing"
	CodeGitTooOld      Code = "git_too_old"
	CodeNotGitRepo     Code = "not_git_repo"
	CodeNoGitHubRemote Code = "no_github_remote"
	CodeNoCommit       Code = "no_commit"
	CodeDirtyTree      Code = "dirty_tree"
	CodeNoGate         Code = "no_gate"
	CodeNoHosts        Code = "no_hosts"
	CodeSkipped        Code = "skipped"

	// The submit answer was lost (the job may exist), and the host-related codes.
	CodeOutcomeUnknown     Code = "outcome_unknown"
	CodeHostReplaced       Code = "host_replaced"
	CodeHostGone           Code = "host_gone"
	CodeNoSuchJob          Code = "no_such_job"
	CodeAmbiguousJob       Code = "ambiguous_job"
	CodeWrongRepo          Code = "wrong_repo"
	CodeConfirmationNeeded Code = "confirmation_required"
	CodeHostUnreachable    Code = "host_unreachable"
	CodeHostState          Code = "host_state"
	CodeRunnerError        Code = "runner_error"
	CodeMissingTool        Code = "missing_tool"
	CodeTooManyHosts       Code = "too_many_hosts"
	CodeBundleFailed       Code = "bundle_failed"
	CodeCacheError         Code = "cache_error"
	CodeLockBusy           Code = "lock_busy"
	CodeCreateFailed       Code = "create_failed"
	CodeCreateUnknown      Code = "create_unknown"
	CodeInterrupted        Code = "interrupted"

	// Patches of gen jobs (M3). patch_refused, patch_conflict and
	// patch_too_large exit 6; patch_mismatch (the sha256 is not the one the
	// host reported) and patch_error (git failed while checking or applying)
	// exit 5. no_patch means the job is not a gen job or did not succeed;
	// job_not_finished means it has not ended yet. stale_base (exit 6) means
	// the working tree is not the commit the job ran on, and the patch touches
	// files that differ between the two.
	CodePatchRefused  Code = "patch_refused"
	CodePatchConflict Code = "patch_conflict"
	CodePatchTooLarge Code = "patch_too_large"
	CodePatchMismatch Code = "patch_mismatch"
	CodePatchError    Code = "patch_error"
	CodeNoPatch       Code = "no_patch"
	CodeJobNotDone    Code = "job_not_finished"
	CodeStaleBase     Code = "stale_base"
)

// Level is the severity of a check.
type Level string

const (
	LevelOK      Level = "ok"
	LevelWarning Level = "warning"
	LevelError   Level = "error"
	LevelSkipped Level = "skipped" // not run because an earlier check failed
)

// Check is one diagnostic line. A warning or an error without a Fix is a bug
// in gatehaul, which Validate reports.
type Check struct {
	Name    string `json:"name"`
	Level   Level  `json:"level"`
	Code    Code   `json:"code,omitempty"`
	Message string `json:"message"`
	Fix     string `json:"fix,omitempty"`
}

// OK returns a passing check.
func OK(name, message string) Check {
	return Check{Name: name, Level: LevelOK, Message: message}
}

// Warn returns a warning: gatehaul can work, but probably not as intended.
func Warn(name string, code Code, message, fix string) Check {
	return Check{Name: name, Level: LevelWarning, Code: code, Message: message, Fix: fix}
}

// Fail returns an error: gatehaul cannot work until it is fixed.
func Fail(name string, code Code, message, fix string) Check {
	return Check{Name: name, Level: LevelError, Code: code, Message: message, Fix: fix}
}

// Skip returns a check that was not run, and names the check it depends on.
func Skip(name, dependsOn string) Check {
	return Check{Name: name, Level: LevelSkipped, Code: CodeSkipped, Message: "not checked: fix " + dependsOn + " first"}
}

// Validate reports a check that would leave the reader without a way forward.
func (c Check) Validate() error {
	switch c.Level {
	case LevelOK, LevelSkipped:
		return nil
	case LevelWarning, LevelError:
		if strings.TrimSpace(c.Fix) == "" {
			return fmt.Errorf("check %q: level %s without a fix", c.Name, c.Level)
		}
		if c.Code == "" {
			return fmt.Errorf("check %q: level %s without a code", c.Name, c.Level)
		}
		return nil
	default:
		return fmt.Errorf("check %q: unknown level %q", c.Name, c.Level)
	}
}

// Error is an error that tells the user what to do next.
type Error struct {
	Code    Code
	Message string
	Fix     string
	Exit    int
	// ID is the job the error is about, when a job already exists: an agent
	// that only reads the JSON can still wait on it.
	ID string
}

func (e *Error) Error() string { return e.Message }

// WriteJSON writes v as indented JSON followed by a newline.
func WriteJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// WriteError prints an error for humans (text) or agents (JSON). The text form
// always ends with a "Fix:" line when a fix is known.
func WriteError(w io.Writer, asJSON bool, e *Error) {
	if asJSON {
		_ = WriteJSON(w, struct {
			Error   bool   `json:"error"`
			Code    Code   `json:"code"`
			Message string `json:"message"`
			Fix     string `json:"fix,omitempty"`
			ID      string `json:"id,omitempty"`
		}{true, e.Code, e.Message, e.Fix, e.ID})
		return
	}
	fmt.Fprintf(w, "error: %s\n", e.Message)
	if e.Fix != "" {
		fmt.Fprintf(w, "Fix: %s\n", e.Fix)
	}
}
