package main

import (
	"bytes"
	"context"
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
)

func cli(env map[string]string, args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := run(args, func(k string) string { return env[k] }, &out, &errOut)
	return code, out.String(), errOut.String()
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

func TestNoArgumentPrintsUsage(t *testing.T) {
	code, out, errOut := cli(nil)
	if code != 2 || out != "" || !strings.Contains(errOut, "usage: sbx-broker") {
		t.Fatalf("code %d, out %q, err %q", code, out, errOut)
	}
}

func TestHelp(t *testing.T) {
	code, out, _ := cli(nil, "help")
	if code != 0 || !strings.Contains(out, "usage: sbx-broker") {
		t.Fatalf("code %d, out %q", code, out)
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{{"nope"}, {"refresh"}, {"status", "a", "b"}, {"reload", "x"}, {"log", "-x"}, {"serve", "x"}} {
		if code, _, errOut := cli(nil, args...); code != 2 || !strings.Contains(errOut, "usage: sbx-broker") {
			t.Errorf("%v: code %d", args, code)
		}
	}
}

func TestNoBroker(t *testing.T) {
	env := map[string]string{"SBX_BROKER_STATE_DIR": stateDir(t)}
	code, _, errOut := cli(env, "status")
	if code != 1 || !strings.Contains(errOut, "no broker runs (start it with: sbx-broker serve)") {
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
	}, {Name: "t2", Listeners: []string{}, Secrets: []secrets.Status{}, Error: "route idp: address already in use"}})
	want := "namespace t1: 1 of 4 secrets ok, listening on 127.0.0.1:29691\n" +
		"  api-key   ok        sbxb_t1_api_key   len 14 sha 1a2b3c4d   fetched 13:02:11\n" +
		"  idp       pending   sbxb_t1_idp\n" +
		"  slow      failed    sbxb_t1_slow      exit 3: oops\n" +
		"  tmo       failed    sbxb_t1_tmo       timeout after 1m0s\n" +
		"namespace t2: 0 of 0 secrets ok, no listeners\n" +
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
