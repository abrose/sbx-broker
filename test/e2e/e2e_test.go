//go:build e2e

// Package e2e proves the broker with the app in a real sandbox: the broker
// and the recorder mock run on this Mac, the test client runs the cases of
// test/cases inside the sandbox check-broker, and the test kit opens only
// the ports of the namespace t1. It needs sbx, the instance check and the
// template sbx-stack-base:0.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/abrose/sbx-broker/internal/eventlog"
	"github.com/abrose/sbx-broker/test/cases"
	"github.com/abrose/sbx-broker/test/harness"
	"github.com/abrose/sbx-broker/test/pgcases"
)

const (
	sandbox    = "check-broker"
	instance   = "check"
	clientName = "sbx-broker-testclient"
	hostAlias  = "host.docker.internal"

	// deadlineMargin is kept free before the test's deadline, so the
	// cleanups (the sandbox within cleanupWait, then the broker and the
	// recorder) finish also when a step runs out of time: the panic of a
	// go test timeout would skip every cleanup.
	deadlineMargin = 5 * time.Minute
	cleanupWait    = 4 * time.Minute
	removeWait     = 3 * time.Minute
	createWait     = 12 * time.Minute
	stepWait       = 2 * time.Minute
	policyWait     = 30 * time.Second
)

func TestEndToEnd(t *testing.T) {
	dir, err := os.Getwd() // go test runs in the package dir: test/e2e
	if err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(dir, "sbxenv.yaml")
	ws := filepath.Join(dir, "ws")
	ctx := runContext(t)

	// Plain sbx is the wrapper of sbx-instances: it picks the instance from
	// the marker file .sbx-instance above this dir. Without it, sbx would
	// reach the default instance, which this test must never touch.
	if _, err := exec.LookPath("sbx-instance"); err != nil {
		t.Fatalf("sbx-instance is not in PATH (%v): the test runs sbx only in the instance %q, "+
			"which the sbx-instances wrapper selects; install the wrapper first", err, instance)
	}
	if out := run(t, ctx, stepWait, dir, nil, "sbx-instance", "which"); firstLine(out) != instance {
		t.Fatalf("sbx-instance which says %q, want %q", firstLine(out), instance)
	}
	start := time.Now()
	run(t, ctx, stepWait, filepath.Join(dir, "..", ".."), []string{"GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0"},
		"go", "build", "-o", filepath.Join(ws, clientName), "./cmd/"+clientName)
	t.Logf("built the test client in %v", since(start))
	run(t, ctx, stepWait, dir, nil, "sbx", "kit", "validate", filepath.Join(dir, "kit"))

	base, records := harness.StartRecorder(t)
	// The Postgres binds 127.0.0.1 on this Mac; the broker dials it, the
	// sandbox never reaches it directly.
	pgUpstream, pgLogs := harness.StartPostgres(t)
	_, state := harness.StartBroker(t, harness.ProofNamespaces(base, pgUpstream))
	harness.ClearRecords(t, records)

	// Registered first: a failed removal of a leftover stops the test, and
	// the cleanup then tries the remove again.
	t.Cleanup(func() { removeSandbox(t, dir, envFile) })
	if listed(t, ctx, dir) {
		t.Logf("a sandbox %s is left from an earlier run: removing it", sandbox)
		run(t, ctx, removeWait, dir, nil, "sbx", "env", "rm", "--force", "--no-input", "--name", sandbox, envFile)
	}

	// sbx keeps the policy log of a removed sandbox under its name: only
	// entries seen after this moment count.
	created := time.Now().Truncate(time.Second)
	run(t, ctx, createWait, dir, nil, "sbx", "env", "create", "--force", "--no-input", "--name", sandbox, envFile)
	t.Logf("created %s in %v", sandbox, since(created))

	start = time.Now()
	out := run(t, ctx, stepWait, dir, nil, "sbx", "exec", sandbox, "--",
		filepath.Join(ws, clientName), "-routes", routesFlag(), "-placeholders", placeholdersFlag())
	t.Logf("ran the cases in the sandbox in %v", since(start))
	outcomes := harness.OutcomesByCase(t, decodeOutcomes[cases.Outcome](t, out))

	// The sandbox reaches the Mac's loopback at host.docker.internal also for
	// raw TCP, through the kit rule localhost:29694. The cancel case opens a
	// second connection to the same host:port, so it needs no other rule.
	start = time.Now()
	pgAddr := fmt.Sprintf("%s:%d", hostAlias, harness.PortT1PG)
	out = run(t, ctx, stepWait, dir, nil, "sbx", "exec", sandbox, "--",
		filepath.Join(ws, clientName), "-postgres", pgAddr, "-placeholders", placeholdersFlag())
	t.Logf("ran the Postgres cases in the sandbox in %v", since(start))
	t.Run("8-postgres", func(t *testing.T) {
		harness.AssertPostgres(t, decodeOutcomes[pgcases.Outcome](t, out), harness.WaitUpstreamLog(pgLogs))
	})

	// The Postgres cases against the closed port 29695 must all end in a
	// connection error or EOF: a bare connect sends no bytes and the broker
	// logs nothing for it, so only real logins can show that sbx forwards the
	// port. A forwarded login would reach the t2 listener (check 1: a
	// placeholder of another namespace) and log a t2 event.
	t.Run("t2-postgres-closed", func(t *testing.T) {
		closed := fmt.Sprintf("%s:%d", hostAlias, harness.PortT2PG)
		out, errOut, err := command(ctx, stepWait, dir, nil, "sbx", "exec", sandbox, "--",
			filepath.Join(ws, clientName), "-postgres", closed, "-placeholders", placeholdersFlag())
		if err != nil {
			t.Fatalf("the test client against %s: %v\nstderr:\n%s", closed, err, errOut)
		}
		got := decodeOutcomes[pgcases.Outcome](t, out)
		if len(got) == 0 {
			t.Fatalf("the test client printed no outcome against %s", closed)
		}
		for _, o := range got {
			t.Logf("case %s against %s: %q", o.Case, closed, o.Message)
			if o.Code != "" || o.Row != "" {
				t.Errorf("case %s against %s: got a Postgres answer (code %q, row %q), want a connection error", o.Case, closed, o.Code, o.Row)
			}
		}
		// The reload and fetch events of t2 at its start are no login.
		for _, e := range brokerEvents(t, state) {
			switch e.Kind {
			case eventlog.KindSwap, eventlog.KindRefuse, eventlog.KindUpstream, eventlog.KindViolation, eventlog.KindClose:
				if e.Namespace == "t2" {
					t.Errorf("broker.log has a login event of namespace t2: %+v", e)
				}
			}
		}
	})

	t.Run("t2-port-closed", func(t *testing.T) {
		url := fmt.Sprintf("http://%s:%d/echo", hostAlias, harness.PortT2API)
		out := run(t, ctx, stepWait, dir, nil, "sbx", "exec", sandbox, "--", "curl", "-s", "-w", `\n%{http_code}`, url)
		body, code, _ := cutLast(out)
		if code != "403" || !strings.Contains(body, "Approval required") {
			t.Errorf("curl %s from the sandbox: status %q, body %q; want 403 from the sbx proxy", url, code, body)
		}
	})

	// After the curl above: a request to the t2 route that got through
	// would show as a record with no case header, which assertion 1 fails.
	harness.AssertProof(t, outcomes, harness.RecordsByCase(harness.Records(t, records)))

	t.Run("policy-log", func(t *testing.T) {
		// No entry is wanted for the blocked raw TCP port 29695: the policy log
		// of this sbx version does not list a blocked raw connect, so t2-postgres-closed
		// proves that block through broker.log alone.
		want := map[string]string{
			fmt.Sprintf("localhost:%d", harness.PortT1API): "allowed",
			fmt.Sprintf("localhost:%d", harness.PortT1IdP): "allowed",
			fmt.Sprintf("localhost:%d", harness.PortT2API): "blocked",
			fmt.Sprintf("localhost:%d", harness.PortT1PG):  "allowed",
		}
		var missing []string
		deadline := time.Now().Add(policyWait)
		for {
			got := policySince(t, ctx, dir, created)
			missing = missing[:0]
			for host, list := range want {
				if !slices.Contains(got[list], host) {
					missing = append(missing, list+" "+host)
				}
			}
			if len(missing) == 0 {
				for _, host := range got["blocked"] {
					t.Logf("blocked since the create: %s", host)
				}
				return
			}
			if time.Now().After(deadline) {
				slices.Sort(missing)
				t.Fatalf("the policy log since %s lacks: %s (it has allowed %q, blocked %q)",
					created.Format(time.RFC3339), strings.Join(missing, ", "), got["allowed"], got["blocked"])
			}
			time.Sleep(2 * time.Second)
		}
	})
}

// runContext ends deadlineMargin before the test's deadline.
func runContext(t *testing.T) context.Context {
	t.Helper()
	end := time.Now().Add(15 * time.Minute)
	if d, ok := t.Deadline(); ok {
		end = d.Add(-deadlineMargin)
	}
	ctx, cancel := context.WithDeadline(context.Background(), end)
	t.Cleanup(cancel)
	return ctx
}

// run runs a command in dir, with env added to this process's, for at most
// wait, and returns its stdout. A failure stops the test with both outputs.
func run(t *testing.T, ctx context.Context, wait time.Duration, dir string, env []string, name string, args ...string) string {
	t.Helper()
	out, errOut, err := command(ctx, wait, dir, env, name, args...)
	if err != nil {
		t.Fatalf("%s %s: %v\nstdout:\n%s\nstderr:\n%s", name, strings.Join(args, " "), err, out, errOut)
	}
	return out
}

func command(ctx context.Context, wait time.Duration, dir string, env []string, name string, args ...string) (stdout, stderr string, err error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.WaitDelay = 5 * time.Second
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err = cmd.Run()
	if ctx.Err() != nil {
		err = fmt.Errorf("%w (%v)", ctx.Err(), err)
	}
	return o.String(), e.String(), err
}

// listed tells whether sbx lists the sandbox.
func listed(t *testing.T, ctx context.Context, dir string) bool {
	t.Helper()
	return slices.Contains(strings.Fields(run(t, ctx, stepWait, dir, nil, "sbx", "ls", "-q")), sandbox)
}

// removeSandbox removes the sandbox and fails the test when sbx still lists
// it. It has its own time, apart from the test's.
func removeSandbox(t *testing.T, dir, envFile string) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupWait)
	defer cancel()
	start := time.Now()
	out, errOut, err := command(ctx, removeWait, dir, nil, "sbx", "env", "rm", "--force", "--no-input", "--name", sandbox, envFile)
	if err != nil {
		t.Logf("sbx env rm %s: %v\nstdout:\n%s\nstderr:\n%s", sandbox, err, out, errOut)
	} else {
		t.Logf("removed %s in %v", sandbox, since(start))
	}
	names, errOut, err := command(ctx, stepWait, dir, nil, "sbx", "ls", "-q")
	switch {
	case err != nil:
		t.Errorf("sbx ls after the remove: %v\n%s", err, errOut)
	case slices.Contains(strings.Fields(names), sandbox):
		t.Errorf("sbx still lists %s after the remove", sandbox)
	}
}

func routesFlag() string {
	return fmt.Sprintf("%s=http://%s:%d,%s=http://%s:%d",
		cases.RouteAPI, hostAlias, harness.PortT1API, cases.RouteIdP, hostAlias, harness.PortT1IdP)
}

func placeholdersFlag() string {
	ph := harness.ProofPlaceholders()
	var pairs []string
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range ph {
			if !yield(k) {
				return
			}
		}
	}) {
		pairs = append(pairs, k+"="+ph[k])
	}
	return strings.Join(pairs, ",")
}

// brokerEvents reads every event of the broker's log.
func brokerEvents(t *testing.T, state string) []eventlog.Event {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(state, "broker.log"))
	if err != nil {
		t.Fatal(err)
	}
	var list []eventlog.Event
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e eventlog.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("broker.log: %v", err)
		}
		list = append(list, e)
	}
	return list
}

// decodeOutcomes reads one JSON outcome per line.
func decodeOutcomes[T any](t *testing.T, out string) []T {
	t.Helper()
	var list []T
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var o T
		if err := json.Unmarshal(sc.Bytes(), &o); err != nil {
			t.Fatalf("the test client printed a line that is no outcome: %q: %v", sc.Text(), err)
		}
		list = append(list, o)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read the test client's output: %v", err)
	}
	return list
}

// policyEntry is one host of `sbx policy log --json`.
type policyEntry struct {
	Host     string `json:"host"`
	LastSeen string `json:"last_seen"`
}

// policySince returns the hosts of the sandbox's policy log, "allowed" and
// "blocked", whose last request was at or after from.
func policySince(t *testing.T, ctx context.Context, dir string, from time.Time) map[string][]string {
	t.Helper()
	out := run(t, ctx, stepWait, dir, nil, "sbx", "policy", "log", sandbox, "--json", "--limit", "0")
	var log map[string][]policyEntry
	if err := json.Unmarshal([]byte(out), &log); err != nil {
		t.Fatalf("sbx policy log: %v\n%s", err, out)
	}
	got := map[string][]string{}
	for _, list := range []string{"allowed", "blocked"} {
		for _, e := range log[list] {
			seen, err := time.Parse(time.RFC3339Nano, e.LastSeen)
			if err != nil {
				t.Fatalf("sbx policy log: last_seen %q of %s: %v", e.LastSeen, e.Host, err)
			}
			if !seen.Before(from) {
				got[list] = append(got[list], e.Host)
			}
		}
	}
	return got
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}

// cutLast splits s at its last newline.
func cutLast(s string) (before, last string, found bool) {
	i := strings.LastIndexByte(s, '\n')
	if i < 0 {
		return "", s, false
	}
	return s[:i], s[i+1:], true
}

func since(t time.Time) time.Duration { return time.Since(t).Round(time.Second) }
