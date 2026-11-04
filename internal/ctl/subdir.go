package ctl

import (
	"fmt"
	"path"
	"strings"

	"github.com/camilbenameur/gatehaul/internal/job"
	"github.com/camilbenameur/gatehaul/internal/out"
)

// cleanSubdir validates the --cd option of run and gen: a directory inside the
// repository, relative to its root. It returns the cleaned path with forward
// slashes, or "" when the command is to run at the root.
func cleanSubdir(dir, verb string) (string, *out.Error) {
	if dir == "" {
		return "", nil
	}
	fix := verb + " --cd src/back-end/src -- <cmd>"
	bad := func(why string) (string, *out.Error) {
		return "", usageError(fmt.Sprintf("--cd %q %s", dir, why), fix)
	}
	for _, r := range dir {
		if r < 0x20 || r == 0x7f {
			return bad("contains a control character")
		}
	}
	d := strings.ReplaceAll(dir, `\`, "/")
	if strings.HasPrefix(d, "/") {
		return bad("is an absolute path; give a path relative to the repository root")
	}
	if len(d) >= 2 && d[1] == ':' && (d[0] >= 'A' && d[0] <= 'Z' || d[0] >= 'a' && d[0] <= 'z') {
		return bad("is an absolute path; give a path relative to the repository root")
	}
	d = path.Clean(d)
	if d == "." {
		return "", nil
	}
	if d == ".." || strings.HasPrefix(d, "../") {
		return bad("leaves the repository; give a path inside it, relative to its root")
	}
	return d, nil
}

// guardedCommand makes the command run in dir, a directory of the sent
// commit. The guard is its own line, so that nothing in cmd (";", "||") can
// run before the cd; a missing directory fails the job with a clear message.
func guardedCommand(dir, sha, cmd string) string {
	msg := fmt.Sprintf("gatehaul: directory %s does not exist in commit %s (--cd is relative to the repository root)", dir, short(sha))
	return "cd -- " + shellQuote(dir) + " 2>/dev/null || { echo " + shellQuote(msg) + " >&2; exit 1; }\n" + cmd
}

// rerun is the command that runs job d again, as the user would type it, with
// self naming gatehaul (Config.Self).
func rerun(self string, d job.Dispatch) string {
	s := commandLine(self, d.Kind)
	if d.Dir != "" {
		s += " --cd " + shellQuote(d.Dir)
	}
	return s + " -- " + shellQuote(d.Cmd)
}

// refName is how the progress line names what is sent.
func refName(fullRef, asked string) string {
	switch {
	case strings.HasPrefix(fullRef, "refs/heads/"):
		return strings.TrimPrefix(fullRef, "refs/heads/")
	case strings.HasPrefix(fullRef, "refs/tags/"):
		return strings.TrimPrefix(fullRef, "refs/tags/")
	case strings.HasPrefix(fullRef, "refs/remotes/"):
		return strings.TrimPrefix(fullRef, "refs/remotes/")
	case fullRef == "HEAD" && asked != "":
		return asked
	}
	return fullRef
}

// sendingLine is the one progress line printed before a job is sent.
func sendingLine(ref, sha, hostName, dir, extra string) string {
	s := fmt.Sprintf("sending %s @ %s to %s", ref, sha[:7], hostName)
	if dir != "" {
		s += ", in " + dir
	}
	return s + extra + "…"
}
