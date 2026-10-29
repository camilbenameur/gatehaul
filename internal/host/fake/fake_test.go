package fake_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/camilbenameur/gatehaul/internal/host"
	"github.com/camilbenameur/gatehaul/internal/host/fake"
	"github.com/camilbenameur/gatehaul/internal/host/hosttest"
)

func TestConformance(t *testing.T) {
	hosttest.Run(t, func(t *testing.T) host.Backend {
		return fake.New(t.TempDir(), 1)
	})
}

var _ hosttest.Injector = (*fake.Backend)(nil)

func TestLifecycle(t *testing.T) {
	ctx := context.Background()
	b := fake.New(t.TempDir(), 2)
	hosts, err := b.Discover(ctx)
	if err != nil || len(hosts) != 2 {
		t.Fatalf("discover = %v, %v; want 2 hosts", hosts, err)
	}
	for i, h := range hosts {
		n, ok := host.ParseDisplayName(h.DisplayName)
		if !ok || n != i+1 || h.Num != n || h.State != host.Running {
			t.Errorf("host %d = %+v", i, h)
		}
	}
	if b.Root(hosts[0]) == b.Root(hosts[1]) {
		t.Errorf("the hosts share the root %s", b.Root(hosts[0]))
	}

	if err := b.Stop(ctx, hosts[0]); err != nil {
		t.Fatalf("stop: %v", err)
	}
	hosts, _ = b.Discover(ctx)
	if hosts[0].State != host.Stopped {
		t.Errorf("state after stop = %s", hosts[0].State)
	}
	if runtime.GOOS != "windows" { // the name bash may resolve to something else there
		// Like a real codespace, the first connection wakes a stopped host.
		if err := b.Transport(hosts[0]).Exec(ctx, strings.NewReader(""), &bytes.Buffer{}); err != nil {
			t.Errorf("exec on a stopped host: %v, want it to wake the host up", err)
		}
		if after, _ := b.Discover(ctx); after[0].State != host.Running {
			t.Errorf("state after the first exec = %s, want running", after[0].State)
		}
	}

	old := hosts[1]
	if err := os.MkdirAll(b.Root(old), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.Root(old), "marker"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete(ctx, old); err != nil {
		t.Fatalf("delete: %v", err)
	}
	err = b.Transport(old).Exec(ctx, strings.NewReader(""), &bytes.Buffer{})
	var ee *host.ExecError
	if !errors.As(err, &ee) {
		t.Errorf("exec on a deleted host: error = %v, want an ExecError", err)
	}
	hosts, _ = b.Discover(ctx)
	if len(hosts) != 1 {
		t.Errorf("hosts after delete = %v, want 1", hosts)
	}
	created, err := b.Create(ctx, host.CreateOptions{Num: 2})
	if err != nil || created.State != host.Running || created.DisplayName != "gatehaul-2" {
		t.Errorf("create = %+v, %v", created, err)
	}
	if created.Name == old.Name {
		t.Errorf("the recreated host keeps the name %q: a replacement cannot be told apart", created.Name)
	}
	if _, err := os.Stat(filepath.Join(b.Root(created), "marker")); err == nil {
		t.Errorf("the recreated host still has the files of the deleted one")
	}
}

func TestCancelledExecIsAnError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the name bash may resolve to something else on Windows")
	}
	b := fake.New(t.TempDir(), 1)
	hosts, _ := b.Discover(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := b.Transport(hosts[0]).Exec(ctx, strings.NewReader("sleep 30\n"), &bytes.Buffer{})
	var ee *host.ExecError
	if !errors.As(err, &ee) || !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want an ExecError wrapping context.Canceled, like the real transport", err)
	}
}

func TestInjectedFailureBeforeStart(t *testing.T) {
	ctx := context.Background()
	b := fake.New(t.TempDir(), 1)
	hosts, _ := b.Discover(ctx)
	b.InjectFailBeforeStart(1)
	var out bytes.Buffer
	if err := b.Transport(hosts[0]).Exec(ctx, strings.NewReader("echo hi\n"), &out); err == nil {
		t.Fatalf("the first exec should fail")
	}
	if out.Len() != 0 {
		t.Errorf("bash ran despite the injected failure: %q", out.String())
	}
	if runtime.GOOS == "windows" {
		t.Skip("the name bash may resolve to something else on Windows")
	}
	if err := b.Transport(hosts[0]).Exec(ctx, strings.NewReader("echo hi\n"), &out); err != nil {
		t.Fatalf("the second exec: %v", err)
	}
	if out.String() != "hi\n" {
		t.Errorf("stdout = %q, want hi", out.String())
	}
}
