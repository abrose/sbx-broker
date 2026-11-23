package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abrose/sbx-broker/internal/admin"
	"github.com/abrose/sbx-broker/internal/eventlog"
	"github.com/abrose/sbx-broker/internal/secrets"
	"github.com/abrose/sbx-broker/internal/server"
	"github.com/abrose/sbx-broker/internal/tui"
)

// noProject is an empty folder with no marker in it: the current folder of
// cli, so that a marker above the real current folder never changes a test.
var noProject string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "sbxb-noproject")
	if err != nil {
		panic(err)
	}
	noProject = dir
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func cli(env map[string]string, args ...string) (int, string, string) {
	return cliIn(noProject, env, args...)
}

// cliIn runs the CLI with dir as the current folder.
func cliIn(dir string, env map[string]string, args ...string) (int, string, string) {
	return cliTUI(dir, env, func(tui.Client, tui.Options) error { return errors.New("no TUI in tests") }, args...)
}

// cliTUI is cliIn with run as the TUI.
func cliTUI(dir string, env map[string]string, run func(tui.Client, tui.Options) error, args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	c := &command{
		getenv:    func(k string) string { return env[k] },
		getwd:     func() (string, error) { return dir, nil },
		runEditor: func(string, []string) error { return errors.New("no editor in tests") },
		tui:       run,
		stdout:    &out,
		stderr:    &errOut,
	}
	code := c.run(args)
	return code, out.String(), errOut.String()
}

// project returns a folder with a marker that names ns.
func project(t *testing.T, ns string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, markerName), ns+"\n", 0o644)
	return dir
}

func stateDir(t *testing.T) string {
	t.Helper()
	state, err := os.MkdirTemp("/tmp", "sbxb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	return state
}

// broker runs a broker with namespace t1 (one ok secret, one route) and
// returns the env for the CLI and the route's port.
func broker(t *testing.T) (map[string]string, int) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "namespaces")
	if err := os.Mkdir(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	ns := fmt.Sprintf("secrets:\n  api-key:\n    command: [\"printf\", \"%%s\", \"real-api-key-1\"]\nhttp:\n  - name: api\n    listen: %d\n    upstream: http://127.0.0.1:9\n", port)
	if err := os.WriteFile(filepath.Join(cfg, "t1.yaml"), []byte(ns), 0o600); err != nil {
		t.Fatal(err)
	}
	state := stateDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = server.Run(ctx, server.Options{ConfigDir: cfg, StateDir: state, FetchTimeout: 10 * time.Second, PollInterval: 100 * time.Millisecond})
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done })
	env := map[string]string{"SBX_BROKER_STATE_DIR": state, "SBX_BROKER_CONFIG_DIR": cfg}
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, out, _ := cli(env, "status")
		if code == 0 && strings.Contains(out, "1 of 1 secrets ok") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the broker did not get ready")
		}
		time.Sleep(50 * time.Millisecond)
	}
	return env, port
}

func TestNoArgumentStartsTUI(t *testing.T) {
	env, _ := broker(t)
	dir := project(t, "t1")
	for _, args := range [][]string{nil, {"tui"}} {
		var got *tui.Options
		var nss []admin.NamespaceStatus
		code, out, errOut := cliTUI(dir, env, func(cl tui.Client, opt tui.Options) error {
			got = &opt
			var err error
			nss, err = cl.Status("")
			return err
		}, args...)
		if code != 0 || out != "" || errOut != "" || got == nil {
			t.Fatalf("%q: code %d, out %q, err %q, ran %v", args, code, out, errOut, got != nil)
		}
		if got.Namespace != "t1" || got.ConfigDir != env["SBX_BROKER_CONFIG_DIR"] || got.Editor == nil {
			t.Fatalf("%q: options %+v", args, *got)
		}
		if len(nss) != 1 || nss[0].Name != "t1" {
			t.Fatalf("%q: the client did not reach the broker: %+v", args, nss)
		}
	}
}

func TestTUIWithoutMarker(t *testing.T) {
	env, _ := broker(t)
	var got *tui.Options
	code, _, errOut := cliTUI(noProject, env, func(_ tui.Client, opt tui.Options) error { got = &opt; return nil })
	if code != 0 || got == nil || got.Namespace != "" {
		t.Fatalf("code %d, err %q, options %+v", code, errOut, got)
	}
}

func TestTUINoBrokerOneLine(t *testing.T) {
	env := map[string]string{"SBX_BROKER_STATE_DIR": stateDir(t)}
	ran := false
	code, out, errOut := cliTUI(noProject, env, func(tui.Client, tui.Options) error { ran = true; return nil })
	if code != 1 || out != "" || errOut != noBroker+"\n" || ran {
		t.Fatalf("code %d, out %q, err %q, ran %v", code, out, errOut, ran)
	}
	if noBroker != "no broker runs (start it with: sbx-broker serve, or as a service with: sbx-broker install)" {
		t.Fatalf("noBroker %q", noBroker)
	}
}

func TestTUIErrorFails(t *testing.T) {
	env, _ := broker(t)
	code, _, errOut := cliTUI(noProject, env, func(tui.Client, tui.Options) error { return errors.New("no tty") })
	if code != 1 || errOut != "sbx-broker: no tty\n" {
		t.Fatalf("code %d, err %q", code, errOut)
	}
}

func TestHelp(t *testing.T) {
	code, out, _ := cli(nil, "help")
	if code != 0 || !strings.Contains(out, "usage: sbx-broker") {
		t.Fatalf("code %d, out %q", code, out)
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{{"nope"}, {"refresh"}, {"status", "a", "b"}, {"reload", "x"}, {"log", "-x"}, {"serve", "x"}, {"tui", "x"}} {
		if code, _, errOut := cli(nil, args...); code != 2 || !strings.Contains(errOut, "usage: sbx-broker") {
			t.Errorf("%v: code %d", args, code)
		}
	}
}

func TestNoBroker(t *testing.T) {
	env := map[string]string{"SBX_BROKER_STATE_DIR": stateDir(t)}
	code, _, errOut := cli(env, "status")
	if code != 1 || errOut != "no broker runs (start it with: sbx-broker serve, or as a service with: sbx-broker install)\n" {
		t.Fatalf("code %d, err %q", code, errOut)
	}
}

func TestStatusAgainstBroker(t *testing.T) {
	env, port := broker(t)
	code, out, _ := cli(env, "status")
	want := fmt.Sprintf("namespace t1: 1 of 1 secrets ok, listening on 127.0.0.1:%d\n", port)
	if code != 0 || !strings.HasPrefix(out, want) {
		t.Fatalf("code %d, out %q", code, out)
	}
	if !strings.Contains(out, "sbxb_t1_api_key") || !strings.Contains(out, "len 14 sha ") {
		t.Fatalf("out %q", out)
	}
	if strings.Contains(out, "real-api-key-1") {
		t.Fatal("status prints the value")
	}
	if code, _, errOut := cli(env, "status", "t9"); code != 1 || !strings.Contains(errOut, "unknown namespace t9") {
		t.Fatalf("code %d, err %q", code, errOut)
	}
}

func TestRefreshAgainstBroker(t *testing.T) {
	env, _ := broker(t)
	if code, _, errOut := cli(env, "refresh", "t1", "nope"); code != 1 || !strings.Contains(errOut, "unknown secret nope") {
		t.Fatalf("code %d, err %q", code, errOut)
	}
	if code, out, _ := cli(env, "refresh", "t1", "api-key"); code != 0 || !strings.Contains(out, "namespace t1:") {
		t.Fatalf("code %d, out %q", code, out)
	}
	if code, _, _ := cli(env, "reload"); code != 0 {
		t.Fatalf("reload: code %d", code)
	}
	code, out, _ := cli(env, "log", "t1")
	if code != 0 || !strings.Contains(out, " fetch t1 ") {
		t.Fatalf("log: code %d, out %q", code, out)
	}
}

func TestFormatStatus(t *testing.T) {
	at := time.Date(2026, 11, 22, 13, 2, 11, 0, time.Local)
	var b bytes.Buffer
	printStatus(&b, []admin.NamespaceStatus{{
		Name:      "t1",
		Listeners: []string{"127.0.0.1:29691"},
		Secrets: []secrets.Status{
			{Name: "api-key", Placeholder: "sbxb_t1_api_key", State: secrets.StateOK, Length: 14, Fingerprint: "1a2b3c4d", FetchedAt: at},
			{Name: "idp", Placeholder: "sbxb_t1_idp", State: secrets.StatePending},
			{Name: "slow", Placeholder: "sbxb_t1_slow", State: secrets.StateFailed, ExitCode: 3, Err: "oops"},
			{Name: "tmo", Placeholder: "sbxb_t1_tmo", State: secrets.StateFailed, ExitCode: -1, Err: "timeout after 1m0s"},
		},
	}, {Name: "t2", Listeners: []string{}, Secrets: []secrets.Status{}, Error: "route idp: address already in use"}}, "/cfg")
	want := "namespace t1: 1 of 4 secrets ok, listening on 127.0.0.1:29691\n" +
		"  file: /cfg/t1.yaml\n" +
		"  api-key   ok        sbxb_t1_api_key   len 14 sha 1a2b3c4d   fetched 13:02:11\n" +
		"  idp       pending   sbxb_t1_idp\n" +
		"  slow      failed    sbxb_t1_slow      exit 3: oops\n" +
		"  tmo       failed    sbxb_t1_tmo       timeout after 1m0s\n" +
		"namespace t2: 0 of 0 secrets ok, no listeners\n" +
		"  file: /cfg/t2.yaml\n" +
		"  error: route idp: address already in use\n"
	if b.String() != want {
		t.Fatalf("got\n%s\nwant\n%s", b.String(), want)
	}
}

func TestFormatLogLine(t *testing.T) {
	at := time.Date(2026, 11, 22, 13, 2, 11, 0, time.Local)
	got := logLine(eventlog.Event{Time: at, Kind: eventlog.KindSwap, Namespace: "t1", Route: "api", Method: "GET", Path: "/v1",
		Status: 200, Secrets: []string{"api-key", "other"}})
	if want := "13:02:11 swap t1 api GET /v1 200 api-key,other -"; got != want {
		t.Fatalf("got %q", got)
	}
	got = logLine(eventlog.Event{Time: at, Kind: eventlog.KindFetch, Namespace: "t1", Secrets: []string{"slow"}, Message: "failed: exit 3"})
	if want := "13:02:11 fetch t1 - - - - slow failed: exit 3"; got != want {
		t.Fatalf("got %q", got)
	}
}

func TestLogLineShowsPostgresUser(t *testing.T) {
	at := time.Date(2026, 11, 22, 13, 2, 11, 0, time.Local)
	got := logLine(eventlog.Event{Time: at, Kind: eventlog.KindSwap, Namespace: "t1", Route: "app", User: "app",
		Secrets: []string{"db-pass"}, Message: "login"})
	if want := "13:02:11 swap t1 app@app - - - db-pass login"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	got = logLine(eventlog.Event{Time: at, Kind: eventlog.KindSwap, Namespace: "t1", Route: "app", Method: "GET", Path: "/"})
	if want := "13:02:11 swap t1 app GET / - - -"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// The path, the method and the message come from a sandboxed app: no
// control sequence reaches the owner's terminal and no line is forged.
func TestLogLineEscapesControls(t *testing.T) {
	at := time.Date(2026, 11, 22, 13, 2, 11, 0, time.Local)
	var b bytes.Buffer
	printEvent(&b, eventlog.Event{Time: at, Kind: eventlog.KindSwap, Namespace: "t1", Route: "api",
		Method: "GE\x1bT x", Path: "/a b\x1b[2J\n13:00:00 swap", Status: 200,
		Message: "msg \x9b31m\x1b[2J\nforged"})
	want := `13:02:11 swap t1 api GE\x1bT%20x /a%20b\x1b[2J\x0a13:00:00%20swap 200 - msg \x9b31m\x1b[2J\x0aforged` + "\n"
	if b.String() != want {
		t.Fatalf("got  %q\nwant %q", b.String(), want)
	}
}

// A failed fetch's stderr line and a namespace error are printed escaped.
func TestStatusEscapesControls(t *testing.T) {
	var b bytes.Buffer
	printStatus(&b, []admin.NamespaceStatus{{Name: "t1", Listeners: []string{}, Error: "bad\x1b[2J",
		Secrets: []secrets.Status{{Name: "a", Placeholder: "sbxb_t1_a", State: secrets.StateFailed, ExitCode: 3, Err: "oops\x1b]0;x\x07"}}}}, "/cfg")
	if strings.ContainsAny(b.String(), "\x1b\x07") {
		t.Fatalf("got %q", b.String())
	}
}

func TestStatusUsesMarker(t *testing.T) {
	env, _ := broker(t)
	dir := project(t, "t1")
	code, out, errOut := cliIn(dir, env, "status")
	head := "marker: " + filepath.Join(dir, markerName) + "\nnamespace t1:"
	if code != 0 || !strings.HasPrefix(out, head) {
		t.Fatalf("code %d, out %q, err %q", code, out, errOut)
	}
	if file := "  file: " + filepath.Join(env["SBX_BROKER_CONFIG_DIR"], "t1.yaml") + "\n"; !strings.Contains(out, file) {
		t.Fatalf("out %q has no %q", out, file)
	}
}

func TestStatusEscapesMarkerPath(t *testing.T) {
	env, _ := broker(t)
	dir := filepath.Join(t.TempDir(), "evil\x1b]0;x\x07")
	writeFile(t, filepath.Join(dir, markerName), "t1\n", 0o644)
	code, out, errOut := cliIn(dir, env, "status")
	if code != 0 || !strings.HasPrefix(out, "marker: ") || strings.ContainsAny(out, "\x1b\x07") {
		t.Fatalf("code %d, out %q, err %q", code, out, errOut)
	}
}

func TestStatusArgBeatsMarker(t *testing.T) {
	env, _ := broker(t)
	code, out, errOut := cliIn(project(t, "t9"), env, "status", "t1")
	if code != 0 || !strings.HasPrefix(out, "namespace t1:") || strings.Contains(out, "marker:") {
		t.Fatalf("code %d, out %q, err %q", code, out, errOut)
	}
}

func TestStatusNoMarkerListsAll(t *testing.T) {
	env, _ := broker(t)
	code, out, _ := cli(env, "status")
	if code != 0 || !strings.HasPrefix(out, "namespace t1:") || strings.Contains(out, "marker:") {
		t.Fatalf("code %d, out %q", code, out)
	}
}

func TestStatusMissingNamespaceHint(t *testing.T) {
	env, _ := broker(t)
	code, _, errOut := cliIn(project(t, "t9"), env, "status")
	if code != 1 || !strings.Contains(errOut, "unknown namespace t9") || !strings.Contains(errOut, "create it with: sbx-broker init t9") {
		t.Fatalf("code %d, err %q", code, errOut)
	}
}

func TestStatusBadMarker(t *testing.T) {
	env, _ := broker(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, markerName), "../x\n", 0o644)
	code, _, errOut := cliIn(dir, env, "status")
	if code != 1 || !strings.Contains(errOut, "not a valid namespace name") || strings.Contains(errOut, "../x") {
		t.Fatalf("code %d, err %q", code, errOut)
	}
}

func TestLogUsesMarker(t *testing.T) {
	env, _ := broker(t)
	code, out, errOut := cliIn(project(t, "t1"), env, "log")
	if code != 0 || out == "" {
		t.Fatalf("code %d, out %q, err %q", code, out, errOut)
	}
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if !strings.Contains(line, " t1 ") {
			t.Fatalf("a line of another namespace: %q", line)
		}
	}
}

func TestRefreshUsesMarker(t *testing.T) {
	env, _ := broker(t)
	code, out, errOut := cliIn(project(t, "t1"), env, "refresh")
	if code != 0 || !strings.Contains(out, "namespace t1:") {
		t.Fatalf("code %d, out %q, err %q", code, out, errOut)
	}
}

func TestRefreshNoMarker(t *testing.T) {
	code, _, errOut := cli(nil, "refresh")
	if code != 2 || !strings.Contains(errOut, "no .sbx-broker marker") || !strings.Contains(errOut, "usage: sbx-broker") {
		t.Fatalf("code %d, err %q", code, errOut)
	}
}
