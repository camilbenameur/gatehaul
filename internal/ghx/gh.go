package ghx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/camilbenameur/gatehaul/internal/out"
)

// MinGHVersion is the oldest gh release gatehaul supports. Older releases used
// a different transport for "gh codespace ssh".
var MinGHVersion = Version{2, 40, 0}

// DisplayPrefix marks the codespaces that gatehaul may use. Their real names
// are random and are rediscovered on every call (docs/SPEC.md §4).
const DisplayPrefix = "gatehaul-"

const (
	localTimeout   = 5 * time.Second
	networkTimeout = 20 * time.Second
)

// Version is a semantic version.
type Version struct{ Major, Minor, Patch int }

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

// Less reports whether v is older than o.
func (v Version) Less(o Version) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Patch < o.Patch
}

var versionRE = regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)

// ParseVersion finds the first x.y[.z] version in s, such as the output of
// "gh --version" or "git --version".
func ParseVersion(s string) (Version, bool) {
	m := versionRE.FindStringSubmatch(s)
	if m == nil {
		return Version{}, false
	}
	var v Version
	v.Major, _ = strconv.Atoi(m[1])
	v.Minor, _ = strconv.Atoi(m[2])
	if m[3] != "" {
		v.Patch, _ = strconv.Atoi(m[3])
	}
	return v, true
}

// GHVersion returns the installed gh version.
func GHVersion(ctx context.Context, r Runner) (Version, error) {
	res, err := r.Run(ctx, []string{"gh", "--version"}, nil, localTimeout)
	if err != nil {
		return Version{}, err
	}
	if !res.OK() {
		return Version{}, fmt.Errorf("gh --version exited with %d: %s", res.ExitCode, firstLine(res.Stderr))
	}
	v, ok := ParseVersion(string(res.Stdout))
	if !ok {
		return Version{}, fmt.Errorf("cannot read a version in %q", firstLine(res.Stdout))
	}
	return v, nil
}

// Account is one entry of "gh auth status --json hosts". gh never puts the
// token itself in this output.
type Account struct {
	State       string `json:"state"`
	Active      bool   `json:"active"`
	Host        string `json:"host"`
	Login       string `json:"login"`
	TokenSource string `json:"tokenSource"`
	Scopes      string `json:"scopes"`
	Error       string `json:"error"`
}

// HasScope reports whether the account's token has scope. ok is false when gh
// does not report scopes (fine-grained tokens, tokens from the environment).
func (a Account) HasScope(scope string) (has, ok bool) {
	if strings.TrimSpace(a.Scopes) == "" {
		return false, false
	}
	for _, s := range strings.Split(a.Scopes, ",") {
		if strings.TrimSpace(s) == scope {
			return true, true
		}
	}
	return false, true
}

// ErrNotLoggedIn means gh has no working account for the host.
var ErrNotLoggedIn = errors.New("gh is not logged in")

// ActiveAccount returns the account gh uses for host. gh exits 0 with --json
// even when authentication fails, so the state field is what counts.
func ActiveAccount(ctx context.Context, r Runner, host string) (Account, error) {
	res, err := r.Run(ctx, []string{"gh", "auth", "status", "--active", "--hostname", host, "--json", "hosts"}, nil, networkTimeout)
	if err != nil {
		return Account{}, err
	}
	if !res.OK() {
		return Account{}, &GHError{Args: "auth status", Result: res}
	}
	var st struct {
		Hosts map[string][]Account `json:"hosts"`
	}
	if err := json.Unmarshal(res.Stdout, &st); err != nil {
		return Account{}, fmt.Errorf("cannot read gh auth status: %w", err)
	}
	for _, a := range st.Hosts[host] {
		if a.Active {
			if a.State != "success" {
				return a, fmt.Errorf("%w: %s", ErrNotLoggedIn, firstNonEmpty(a.Error, a.State))
			}
			return a, nil
		}
	}
	return Account{}, ErrNotLoggedIn
}

// Codespace is one entry of "gh codespace list --json".
type Codespace struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	State       string `json:"state"`
	Repository  string `json:"repository"`
	MachineName string `json:"machineName"`
	LastUsedAt  string `json:"lastUsedAt"`
}

// ListHosts returns the codespaces of repo whose display name starts with
// DisplayPrefix.
func ListHosts(ctx context.Context, r Runner, repo string) ([]Codespace, error) {
	res, err := r.Run(ctx, []string{
		"gh", "codespace", "list", "--repo", repo, "--limit", "100",
		"--json", "name,displayName,state,repository,machineName,lastUsedAt",
	}, nil, networkTimeout)
	if err != nil {
		return nil, err
	}
	if !res.OK() {
		return nil, &GHError{Args: "codespace list", Result: res}
	}
	var all []Codespace
	if err := json.Unmarshal(res.Stdout, &all); err != nil {
		return nil, fmt.Errorf("cannot read gh codespace list: %w", err)
	}
	var hosts []Codespace
	for _, c := range all {
		if strings.HasPrefix(c.DisplayName, DisplayPrefix) {
			hosts = append(hosts, c)
		}
	}
	return hosts, nil
}

// GHError is a gh command that ran and failed.
type GHError struct {
	Args   string
	Result Result
}

func (e *GHError) Error() string {
	return fmt.Sprintf("gh %s exited with %d: %s", e.Args, e.Result.ExitCode, firstLine(e.Result.Stderr))
}

// Classify maps a gh error message to a stable code and the command that fixes
// it. Unknown messages map to CodeGHError, with a generic fix.
func Classify(host, stderr string) (out.Code, string) {
	s := strings.ToLower(stderr)
	switch {
	case strings.Contains(s, "unknown flag") || strings.Contains(s, "unknown command"):
		return out.CodeGHTooOld, "upgrade gh to " + MinGHVersion.String() + " or later: https://cli.github.com"
	case strings.Contains(s, "codespace") && (strings.Contains(s, "scope") || strings.Contains(s, "http 403")):
		return out.CodeScopeMissing, "gh auth refresh -h " + host + " -s codespace"
	case strings.Contains(s, "gh auth login") || strings.Contains(s, "not logged") || strings.Contains(s, "http 401") || strings.Contains(s, "bad credentials"):
		return out.CodeNotLoggedIn, "gh auth login -h " + host
	case strings.Contains(s, "rate limit"):
		return out.CodeRateLimited, "wait a minute, then retry"
	case strings.Contains(s, "could not resolve") || strings.Contains(s, "no such host") ||
		strings.Contains(s, "dial tcp") || strings.Contains(s, "connection refused") || strings.Contains(s, "timeout"):
		return out.CodeNetwork, "check the network connection to " + host + ", then retry"
	default:
		return out.CodeGHError, "run the gh command shown in the message by hand to see the full error"
	}
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
