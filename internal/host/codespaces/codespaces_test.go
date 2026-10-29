package codespaces

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/camilbenameur/gatehaul/internal/clock"
	"github.com/camilbenameur/gatehaul/internal/ghx"
	"github.com/camilbenameur/gatehaul/internal/host"
	"github.com/camilbenameur/gatehaul/internal/out"
)

const listJSON = `[
 {"name":"zeta-1","displayName":"gatehaul-2","state":"Shutdown","repository":"o/r","machineName":"basicLinux32gb","lastUsedAt":"2026-10-29T10:00:00Z"},
 {"name":"alpha-9","displayName":"gatehaul-1","state":"Available","repository":"o/r","machineName":"basicLinux32gb","lastUsedAt":"2026-10-29T11:00:00Z"},
 {"name":"other","displayName":"gatehaul-x","state":"Available","repository":"o/r"},
 {"name":"mine","displayName":"my-box","state":"Available","repository":"o/r"}
]`

var listPrefix = []string{"gh", "codespace", "list", "--repo", "o/r"}

func newBackend(f *ghx.Fake) (*Backend, *clock.Fake) {
	b := New(f, "o/r")
	b.GHHost = "github.com"
	clk := clock.NewFake(time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC))
	b.Clock = clk
	return b, clk
}

func TestMapStateTable(t *testing.T) {
	cases := []struct {
		raw  string
		want host.State
	}{
		{"Available", host.Running},
		{"Starting", host.Busy},
		{"ShuttingDown", host.Busy},
		{"Provisioning", host.Busy},
		{"Rebuilding", host.Busy},
		{"Queued", host.Busy},
		{"Awaiting", host.Busy},
		{"Exporting", host.Busy},
		{"Updating", host.Busy},
		{"Unavailable", host.Busy},
		{"Created", host.Busy},
		{"Shutdown", host.Stopped},
		{"Archived", host.Stopped},
		{"Deleted", host.NotFound},
		{"Moved", host.NotFound},
		{"Failed", host.Failed},
	}
	for _, c := range cases {
		got, err := MapState(c.raw)
		if err != nil || got != c.want {
			t.Errorf("MapState(%q) = %q, %v; want %q", c.raw, got, err, c.want)
		}
	}
	// Anything the table does not hold must be an error: never Running.
	for _, raw := range []string{"Unknown", "available", "", "Whatever"} {
		if got, err := MapState(raw); err == nil {
			t.Errorf("MapState(%q) = %q, want an error", raw, got)
		}
	}
}

func TestDiscoverFiltersSortsAndCaches(t *testing.T) {
	f := (&ghx.Fake{}).On(listPrefix, ghx.Stdout(listJSON), nil)
	b, clk := newBackend(f)
	ctx := context.Background()

	hosts, err := b.Discover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 2 || hosts[0].DisplayName != "gatehaul-1" || hosts[1].DisplayName != "gatehaul-2" {
		t.Fatalf("hosts = %+v, want gatehaul-1 then gatehaul-2", hosts)
	}
	if hosts[0].Name != "alpha-9" || hosts[0].Num != 1 || hosts[0].State != host.Running || hosts[0].RawState != "Available" {
		t.Errorf("host 1 = %+v", hosts[0])
	}
	if hosts[1].State != host.Stopped || hosts[1].Machine != "basicLinux32gb" || hosts[1].LastUsed.IsZero() {
		t.Errorf("host 2 = %+v", hosts[1])
	}

	if _, err := b.Discover(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(f.Calls()); n != 1 {
		t.Fatalf("second Discover within 60 s made %d gh calls, want 1", n)
	}
	clk.Advance(61 * time.Second)
	if _, err := b.Discover(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(f.Calls()); n != 2 {
		t.Fatalf("Discover after 61 s made %d gh calls in total, want 2", n)
	}
	b.Invalidate()
	if _, err := b.Discover(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(f.Calls()); n != 3 {
		t.Fatalf("Discover after Invalidate made %d gh calls in total, want 3", n)
	}
}

func TestDiscoverUnknownStateOnlyAffectsThatHost(t *testing.T) {
	f := (&ghx.Fake{}).On(listPrefix, ghx.Stdout(`[
	 {"name":"n1","displayName":"gatehaul-1","state":"Hibernating"},
	 {"name":"n2","displayName":"gatehaul-2","state":"Available"},
	 {"name":"n3","displayName":"gatehaul-3","state":"Failed"}]`), nil)
	b, _ := newBackend(f)
	hosts, err := b.Discover(context.Background())
	if err != nil || len(hosts) != 3 {
		t.Fatalf("hosts = %+v, %v; one odd codespace must not hide the others", hosts, err)
	}
	if hosts[0].State != host.Unknown || hosts[0].RawState != "Hibernating" {
		t.Errorf("host 1 = %+v, want the unknown state, never running", hosts[0])
	}
	if hosts[1].State != host.Running {
		t.Errorf("host 2 = %+v", hosts[1])
	}
	if hosts[2].State != host.Failed {
		t.Errorf("host 3 = %+v", hosts[2])
	}
}

func TestDiscoverRefusesTwoCodespacesWithTheSameNumber(t *testing.T) {
	f := (&ghx.Fake{}).On(listPrefix, ghx.Stdout(`[
	 {"name":"first","displayName":"gatehaul-1","state":"Available"},
	 {"name":"second","displayName":"gatehaul-1","state":"Shutdown"}]`), nil)
	b, _ := newBackend(f)
	_, err := b.Discover(context.Background())
	var oe *out.Error
	if !errors.As(err, &oe) || oe.Code != out.CodeHostState || !strings.Contains(oe.Fix, "gh codespace delete -c second") || oe.Exit != out.ExitInfra {
		t.Fatalf("err = %v, want an out.Error host_state that says which one to delete", err)
	}
}

func TestCreateThatTimesOutMayHaveCreatedTheCodespace(t *testing.T) {
	f := (&ghx.Fake{}).On([]string{"gh", "codespace", "create"}, ghx.Result{}, context.DeadlineExceeded)
	b, _ := newBackend(f)
	_, err := b.Create(context.Background(), host.CreateOptions{Num: 1, Repo: "o/r", Branch: "main", Machine: "m", IdleTimeout: time.Hour, Retention: time.Hour})
	var oe *out.Error
	if !errors.As(err, &oe) || oe.Code != out.CodeCreateUnknown || !strings.Contains(oe.Fix, "gh codespace list --repo o/r") {
		t.Fatalf("err = %v, want create_unknown with a fix that checks the list", err)
	}
}

func TestDiscoverClassifiesGHFailures(t *testing.T) {
	f := (&ghx.Fake{}).On(listPrefix, ghx.Exit(1, "HTTP 403: Must have admin rights; needs the codespace scope"), nil)
	b, _ := newBackend(f)
	_, err := b.Discover(context.Background())
	var oe *out.Error
	if !errors.As(err, &oe) || oe.Code != out.CodeScopeMissing || !strings.Contains(oe.Fix, "gh auth refresh") {
		t.Fatalf("err = %v, want scope_missing with a gh auth refresh fix", err)
	}

	f = (&ghx.Fake{}).On(listPrefix, ghx.Result{}, errors.New("exec: \"gh\": executable file not found in $PATH"))
	b, _ = newBackend(f)
	if _, err = b.Discover(context.Background()); !errors.As(err, &oe) || oe.Code != out.CodeGHError {
		t.Fatalf("unknown error: err = %v, want gh_error", err)
	}
}

func TestDiscoverPassesCancellationThrough(t *testing.T) {
	f := (&ghx.Fake{}).On(listPrefix, ghx.Stdout("[]"), nil)
	b, _ := newBackend(f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.Discover(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestCreateWithExplicitOptions(t *testing.T) {
	f := (&ghx.Fake{}).
		On([]string{"gh", "codespace", "create"}, ghx.Stdout("cs-new-123\n"), nil)
	b, _ := newBackend(f)
	h, err := b.Create(context.Background(), host.CreateOptions{
		Num: 2, Repo: "o/r", Branch: "trunk", Machine: "basicLinux32gb",
		IdleTimeout: 30 * time.Minute, Retention: 72 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"gh", "codespace", "create", "-R", "o/r", "-b", "trunk", "-m", "basicLinux32gb", "-d", "gatehaul-2",
		"--idle-timeout", "30m", "--retention-period", "72h", "--default-permissions",
	}
	if got := f.Calls(); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if h.Name != "cs-new-123" || h.DisplayName != "gatehaul-2" || h.Num != 2 {
		t.Fatalf("host = %+v", h)
	}
}

func TestCreateLooksUpBranchAndMachine(t *testing.T) {
	f := (&ghx.Fake{}).
		On([]string{"gh", "repo", "view", "o/r"}, ghx.Stdout(`{"defaultBranchRef":{"name":"main"}}`), nil).
		On([]string{"gh", "api", "repos/o/r/codespaces/machines"}, ghx.Stdout(`{"total_count":3,"machines":[
			{"name":"standardLinux32gb","cpus":4,"memory_in_bytes":17179869184},
			{"name":"basicLinux32gb","cpus":2,"memory_in_bytes":8589934592},
			{"name":"premiumLinux","cpus":8,"memory_in_bytes":34359738368}]}`), nil).
		On([]string{"gh", "codespace", "create"}, ghx.Stdout("some message\ncs-name\n"), nil)
	b, _ := newBackend(f)
	h, err := b.Create(context.Background(), host.CreateOptions{Num: 1, IdleTimeout: 5 * time.Minute, Retention: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	calls := f.Calls()
	last := strings.Join(calls[len(calls)-1], " ")
	for _, want := range []string{"-R o/r", "-b main", "-m basicLinux32gb", "-d gatehaul-1", "--idle-timeout 5m", "--retention-period 24h", "--default-permissions"} {
		if !strings.Contains(last, want) {
			t.Errorf("create command %q lacks %q", last, want)
		}
	}
	if h.Name != "cs-name" {
		t.Errorf("name = %q, want the last line of stdout", h.Name)
	}
}

func TestCreateErrors(t *testing.T) {
	f := (&ghx.Fake{}).
		On([]string{"gh", "codespace", "create"}, ghx.Exit(1, "HTTP 401: Bad credentials"), nil)
	b, _ := newBackend(f)
	_, err := b.Create(context.Background(), host.CreateOptions{Num: 1, Branch: "main", Machine: "m", IdleTimeout: time.Hour, Retention: time.Hour})
	var oe *out.Error
	if !errors.As(err, &oe) || oe.Code != out.CodeNotLoggedIn || oe.Fix == "" {
		t.Fatalf("err = %v, want not_logged_in with a fix", err)
	}

	f = (&ghx.Fake{}).On([]string{"gh", "codespace", "create"}, ghx.Stdout("\n"), nil)
	b, _ = newBackend(f)
	_, err = b.Create(context.Background(), host.CreateOptions{Num: 1, Branch: "main", Machine: "m", IdleTimeout: time.Hour, Retention: time.Hour})
	if !errors.As(err, &oe) || oe.Code != out.CodeCreateFailed {
		t.Fatalf("empty name: err = %v, want create_failed", err)
	}

	f = (&ghx.Fake{}).On([]string{"gh", "api"}, ghx.Stdout(`{"machines":[]}`), nil)
	b, _ = newBackend(f)
	_, err = b.Create(context.Background(), host.CreateOptions{Num: 1, Branch: "main", IdleTimeout: time.Hour, Retention: time.Hour})
	if !errors.As(err, &oe) || oe.Code != out.CodeCreateFailed || !strings.Contains(oe.Fix, "--machine") {
		t.Fatalf("no machines: err = %v, want create_failed with a --machine fix", err)
	}
}

func TestStopAndDeleteArgvAndCacheInvalidation(t *testing.T) {
	f := (&ghx.Fake{}).
		On(listPrefix, ghx.Stdout(listJSON), nil).
		On([]string{"gh", "codespace", "stop"}, ghx.Result{}, nil).
		On([]string{"gh", "codespace", "delete"}, ghx.Result{}, nil)
	b, _ := newBackend(f)
	ctx := context.Background()
	h := host.Host{Name: "cs-1", DisplayName: "gatehaul-1", Num: 1}
	if _, err := b.Discover(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Stop(ctx, h); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete(ctx, h); err != nil {
		t.Fatal(err)
	}
	calls := f.Calls()
	if !reflect.DeepEqual(calls[1], []string{"gh", "codespace", "stop", "-c", "cs-1"}) {
		t.Errorf("stop argv = %v", calls[1])
	}
	if !reflect.DeepEqual(calls[2], []string{"gh", "codespace", "delete", "-c", "cs-1", "--force"}) {
		t.Errorf("delete argv = %v", calls[2])
	}
	if _, err := b.Discover(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(f.Calls()); n != 4 {
		t.Errorf("Discover after Stop and Delete used the stale list: %d calls, want 4", n)
	}
}

func TestStopFailureHasAFix(t *testing.T) {
	f := (&ghx.Fake{}).On([]string{"gh", "codespace", "stop"}, ghx.Exit(1, "boom"), nil)
	b, _ := newBackend(f)
	err := b.Stop(context.Background(), host.Host{Name: "cs-1"})
	var oe *out.Error
	if !errors.As(err, &oe) || oe.Code != out.CodeGHError || oe.Fix == "" {
		t.Fatalf("err = %v, want gh_error with a fix", err)
	}
}

func TestRootAndTransport(t *testing.T) {
	b, _ := newBackend(&ghx.Fake{})
	h := host.Host{Name: "cs-1"}
	if b.Root(h) != "/workspaces/.gatehaul" {
		t.Errorf("Root = %q", b.Root(h))
	}
	if b.Transport(h) == nil {
		t.Error("Transport returned nil")
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Minute, "30m"},
		{72 * time.Hour, "72h"},
		{5 * time.Minute, "5m"},
		{90 * time.Second, "1m30s"},
		{24 * time.Hour, "24h"},
		{time.Hour + time.Minute, "61m"},
	}
	for _, c := range cases {
		if got := FormatDuration(c.d); got != c.want {
			t.Errorf("FormatDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}
