package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abrose/sbx-broker/internal/admin"
	"github.com/abrose/sbx-broker/internal/eventlog"
	"github.com/abrose/sbx-broker/internal/launchd"
	"github.com/abrose/sbx-broker/internal/secrets"
	"github.com/abrose/sbx-broker/internal/server"
	"github.com/abrose/sbx-broker/internal/tui"
)

// noProject is an empty folder with no marker in it: the current folder of
// cli, so that a marker above the real current folder never changes a test.
// fakeHome is the home dir of cli, so that no test reads or writes the
// owner's real dirs or ~/Library/LaunchAgents.
var noProject, fakeHome string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "sbxb-noproject")
	if err != nil {
		panic(err)
	}
	home, err := os.MkdirTemp("", "sbxb-home")
	if err != nil {
		panic(err)
	}
	noProject, fakeHome = dir, home
	code := m.Run()
	_ = os.RemoveAll(dir)
	_ = os.RemoveAll(home)
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
	c, out, errOut := testCommand(dir, env)
	c.tui = run
	code := c.run(args)
	return code, out.String(), errOut.String()
}

// testCommand returns a command with dir as the current folder, fakeHome as
// the home dir, and no editor, TUI or launchctl.
func testCommand(dir string, env map[string]string) (c *command, out, errOut *bytes.Buffer) {
	out, errOut = &bytes.Buffer{}, &bytes.Buffer{}
	c = &command{
		getenv:     func(k string) string { return env[k] },
		getwd:      func() (string, error) { return dir, nil },
		home:       func() (string, error) { return fakeHome, nil },
		runEditor:  func(string, []string) error { return errors.New("no editor in tests") },
		tui:        func(tui.Client, tui.Options) error { return errors.New("no TUI in tests") },
		launchctl:  func(...string) ([]byte, error) { return nil, errors.New("no launchctl in tests") },
		agentWait:  time.Second,
		pidPoll:    10 * time.Millisecond,
		pidWait:    200 * time.Millisecond,
		answerWait: time.Second,
		stdout:     out,
		stderr:     errOut,
	}
	return c, out, errOut
}

// fakeLaunchctl knows one agent: print succeeds while it is loaded,
// bootstrap loads it (and calls onBootstrap, when set) and bootout unloads it.
type fakeLaunchctl struct {
	loaded       bool
	pid          int   // the pid that print shows for a loaded agent; 0: none
	pidSeq       []int // after bootstrap, each print takes the next pid from here first
	printErr     error // when set, print fails with it
	bootstrapped bool
	calls        []string
	onBootstrap  func()
}

func (f *fakeLaunchctl) run(args ...string) ([]byte, error) {
	f.calls = append(f.calls, args[0])
	switch args[0] {
	case "print":
		if f.printErr != nil {
			return []byte("print failed"), f.printErr
		}
		if !f.loaded {
			return nil, &launchd.ExitError{Code: 113}
		}
		pid := f.pid
		if f.bootstrapped && len(f.pidSeq) > 0 {
			pid, f.pidSeq = f.pidSeq[0], f.pidSeq[1:]
		}
		if pid > 0 {
			return []byte(fmt.Sprintf("service = {\n\tstate = running\n\tpid = %d\n}\n", pid)), nil
		}
	case "bootstrap":
		f.loaded, f.bootstrapped = true, true
		if f.onBootstrap != nil {
			f.onBootstrap()
		}
	case "bootout":
		f.loaded = false
	}
	return nil, nil
}

// agentCLI runs install or uninstall with f as launchctl, home as the home
// dir and dir as the current folder. It returns the plist path too.
func agentCLI(t *testing.T, dir, home string, env map[string]string, f *fakeLaunchctl, args ...string) (code int, out, errOut, plist string) {
	t.Helper()
	c, o, e := testCommand(dir, env)
	c.home = func() (string, error) { return home, nil }
	c.launchctl = f.run
	c.agentWait = 300 * time.Millisecond
	code = c.run(args)
	return code, o.String(), e.String(), filepath.Join(home, "Library", "LaunchAgents", "io.github.abrose.sbx-broker.plist")
}

func TestInstallRefusesTerminalBroker(t *testing.T) {
	env, _ := broker(t)
	f := &fakeLaunchctl{}
	code, out, errOut, plist := agentCLI(t, noProject, t.TempDir(), env, f, "install")
	sock := filepath.Join(env["SBX_BROKER_STATE_DIR"], "admin.sock")
	want := "sbx-broker: a broker already runs on " + sock + ", not as the launchd agent: stop it, then run sbx-broker install again\n"
	if code != 1 || out != "" || errOut != want {
		t.Fatalf("code %d, out %q, err %q", code, out, errOut)
	}
	if _, err := os.Stat(plist); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a plist was written: %v", err)
	}
	if !slices.Equal(f.calls, []string{"print"}) {
		t.Fatalf("calls %q", f.calls)
	}
}

// silent listens on the admin socket in state, accepts, and never answers:
// a serve stopped with ctrl+z, or a broker that hangs.
func silent(t *testing.T, state string) {
	t.Helper()
	ln, err := net.Listen("unix", admin.SocketPath(state))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
}

// within runs fn and fails the test when it does not return in d.
func within(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return in %v", what, d)
	}
}

// A broker that accepts and never answers still holds the lock: install
// refuses, as it does for a terminal broker (C8), and writes no plist.
func TestInstallRefusesSilentBroker(t *testing.T) {
	state := stateDir(t)
	silent(t, state)
	f := &fakeLaunchctl{}
	var code int
	var out, errOut, plist string
	within(t, 3*time.Second, "install", func() {
		code, out, errOut, plist = agentCLI(t, noProject, t.TempDir(), map[string]string{"SBX_BROKER_STATE_DIR": state}, f, "install")
	})
	want := "sbx-broker: a broker holds " + filepath.Join(state, "admin.sock") + " but does not answer: stop it, then run sbx-broker install again\n"
	if code != 1 || out != "" || errOut != want {
		t.Fatalf("code %d, out %q, err %q", code, out, errOut)
	}
	if _, err := os.Stat(plist); !errors.Is(err, os.ErrNotExist) || len(f.calls) != 0 {
		t.Fatalf("plist %v, calls %q", err, f.calls)
	}
}

// The agent's broker accepts but never answers: install gives up after its
// wait, however long one probe could wait.
func TestInstallWaitIsBounded(t *testing.T) {
	state := stateDir(t)
	home := t.TempDir()
	f := &fakeLaunchctl{onBootstrap: func() { silent(t, state) }}
	c, out, errOut := testCommand(noProject, map[string]string{"SBX_BROKER_STATE_DIR": state})
	c.home = func() (string, error) { return home, nil }
	c.launchctl = f.run
	c.agentWait = 300 * time.Millisecond
	c.answerWait = 5 * time.Second
	var code int
	within(t, 3*time.Second, "install", func() { code = c.run([]string{"install"}) })
	plist := filepath.Join(home, "Library", "LaunchAgents", "io.github.abrose.sbx-broker.plist")
	want := "installed " + plist + ", but the broker does not answer; see " + filepath.Join(state, "serve.log") + "\n"
	if code != 1 || out.String() != "" || errOut.String() != want {
		t.Fatalf("code %d, out %q, err %q", code, out, errOut)
	}
}

func TestInstallCapturesShellEnv(t *testing.T) {
	wd := stateDir(t)
	home := t.TempDir()
	env := map[string]string{
		"PATH":                     "/opt/homebrew/bin:/usr/bin:/bin",
		"SBX_BROKER_STATE_DIR":     "s",
		"SBX_BROKER_CONFIG_DIR":    "cfg",
		"SBX_BROKER_FETCH_TIMEOUT": "90s",
		"PGPASSWORD":               "not-captured",
		"HOME":                     "/not/captured",
	}
	f := &fakeLaunchctl{}
	code, out, errOut, plist := agentCLI(t, wd, home, env, f, "install")
	state := filepath.Join(wd, "s")
	if want := "installed " + plist + ", but the broker does not answer; see " + filepath.Join(state, "serve.log") + "\n"; code != 1 || out != "" || errOut != want {
		t.Fatalf("code %d, out %q, err %q", code, out, errOut)
	}
	if !slices.Equal(f.calls, []string{"print", "bootstrap"}) {
		t.Fatalf("calls %q", f.calls)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		t.Fatal(err)
	}
	want := launchd.Agent{
		Label:     "io.github.abrose.sbx-broker",
		PlistPath: plist,
		Program:   exe,
		Env: map[string]string{
			"PATH":                     "/opt/homebrew/bin:/usr/bin:/bin",
			"SBX_BROKER_STATE_DIR":     state,
			"SBX_BROKER_CONFIG_DIR":    filepath.Join(wd, "cfg"),
			"SBX_BROKER_FETCH_TIMEOUT": "90s",
		},
		LogPath: filepath.Join(state, "serve.log"),
	}.Plist()
	got, err := os.ReadFile(plist)
	if err != nil || string(got) != string(want) {
		t.Fatalf("plist %v:\n%s\nwant\n%s", err, got, want)
	}
}

// With no SBX_BROKER_* set, the plist names no dirs and the log goes to the
// default state dir under the home dir.
func TestInstallDefaultDirs(t *testing.T) {
	home := t.TempDir()
	f := &fakeLaunchctl{}
	code, _, errOut, plist := agentCLI(t, noProject, home, map[string]string{"PATH": "/usr/bin:/bin"}, f, "install")
	log := filepath.Join(home, ".local", "state", "sbx-broker", "serve.log")
	if code != 1 || !strings.HasSuffix(errOut, "see "+log+"\n") {
		t.Fatalf("code %d, err %q", code, errOut)
	}
	got, err := os.ReadFile(plist)
	if err != nil || strings.Contains(string(got), "SBX_BROKER_") || !strings.Contains(string(got), "<string>"+log+"</string>") {
		t.Fatalf("plist %v:\n%s", err, got)
	}
}

// A loaded agent that answers is replaced: booted out and bootstrapped again.
func TestInstallReplacesLoadedAgent(t *testing.T) {
	env, _ := broker(t)
	f := &fakeLaunchctl{loaded: true, pid: 4711}
	code, out, errOut, plist := agentCLI(t, noProject, t.TempDir(), env, f, "install")
	sock := filepath.Join(env["SBX_BROKER_STATE_DIR"], "admin.sock")
	if want := "installed " + plist + "\nthe broker answers on " + sock + "\n"; code != 0 || out != want || errOut != "" {
		t.Fatalf("code %d, out %q, err %q", code, out, errOut)
	}
	if want := []string{"print", "print", "bootout", "print", "bootstrap", "print", "print"}; !slices.Equal(f.calls, want) {
		t.Fatalf("calls %q, want %q", f.calls, want)
	}
}

// An answering broker is not the agent when the agent has no running
// process: install does not count it as the agent's broker (#35).
func TestInstallDoesNotCountOtherBroker(t *testing.T) {
	var crashLoop []int
	for pid := 100; pid < 200; pid++ {
		crashLoop = append(crashLoop, pid)
	}
	cases := map[string]*fakeLaunchctl{
		"no pid":                       {loaded: true},
		"a pid at first, then none":    {loaded: true, pidSeq: []int{4711}}, // the new agent process exits on the lock
		"a pid that changes each read": {loaded: true, pidSeq: crashLoop},
	}
	for name, f := range cases {
		env, _ := broker(t)
		code, out, errOut, _ := agentCLI(t, noProject, t.TempDir(), env, f, "install")
		want := "sbx-broker: a broker answers, but it is not the launchd agent (the agent has no running process): stop the other broker, then run sbx-broker install again\n"
		if code != 1 || out != "" || errOut != want {
			t.Fatalf("%s: code %d, out %q, err %q", name, code, out, errOut)
		}
	}
}

// A print that fails for any reason but "not found" is an error: install and
// uninstall exit 1, name the launchctl error and never bootstrap.
func TestInstallReportsPrintError(t *testing.T) {
	for _, cmd := range []string{"install", "uninstall"} {
		f := &fakeLaunchctl{printErr: &launchd.ExitError{Code: 5}}
		env := map[string]string{"SBX_BROKER_STATE_DIR": stateDir(t)}
		code, out, errOut, _ := agentCLI(t, noProject, t.TempDir(), env, f, cmd)
		if code != 1 || out != "" || !strings.Contains(errOut, "launchctl print") || !strings.Contains(errOut, "print failed") {
			t.Fatalf("%s: code %d, out %q, err %q", cmd, code, out, errOut)
		}
		if slices.Contains(f.calls, "bootstrap") || slices.Contains(f.calls, "bootout") {
			t.Fatalf("%s: calls %q", cmd, f.calls)
		}
	}
	// with a broker that answers, the pre-check reports it too
	env, _ := broker(t)
	f := &fakeLaunchctl{printErr: &launchd.ExitError{Code: 5}}
	code, _, errOut, _ := agentCLI(t, noProject, t.TempDir(), env, f, "install")
	if code != 1 || !strings.Contains(errOut, "launchctl print") || slices.Contains(f.calls, "bootstrap") {
		t.Fatalf("pre-check: code %d, err %q, calls %q", code, errOut, f.calls)
	}
}

// A bad fetch timeout would make the agent's serve exit at once, again and
// again: install refuses it before it writes a plist.
func TestInstallRefusesBadFetchTimeout(t *testing.T) {
	f := &fakeLaunchctl{}
	env := map[string]string{"SBX_BROKER_STATE_DIR": stateDir(t), "SBX_BROKER_FETCH_TIMEOUT": "soon"}
	code, _, errOut, plist := agentCLI(t, noProject, t.TempDir(), env, f, "install")
	if code != 1 || errOut != "sbx-broker: SBX_BROKER_FETCH_TIMEOUT \"soon\" is not a positive Go duration\n" {
		t.Fatalf("code %d, err %q", code, errOut)
	}
	if _, err := os.Stat(plist); !errors.Is(err, os.ErrNotExist) || len(f.calls) != 0 {
		t.Fatalf("plist %v, calls %q", err, f.calls)
	}
}

func TestInstallLaunchctlFails(t *testing.T) {
	f := func(args ...string) ([]byte, error) {
		if args[0] == "bootstrap" {
			return []byte("Bootstrap failed: 5: Input/output error\n"), errors.New("exit status 5")
		}
		return nil, &launchd.ExitError{Code: 113}
	}
	home := t.TempDir()
	c, out, errOut := testCommand(noProject, map[string]string{"SBX_BROKER_STATE_DIR": stateDir(t)})
	c.home = func() (string, error) { return home, nil }
	c.launchctl = f
	plist := filepath.Join(home, "Library", "LaunchAgents", "io.github.abrose.sbx-broker.plist")
	if code := c.run([]string{"install"}); code != 1 || out.String() != "" || !strings.Contains(errOut.String(), "Bootstrap failed: 5") ||
		!strings.Contains(errOut.String(), "the plist stays at "+plist+", sbx-broker uninstall removes it") {
		t.Fatalf("code %d, out %q, err %q", code, out, errOut)
	}
}

func TestUninstallOutput(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"SBX_BROKER_STATE_DIR": stateDir(t)}
	f := &fakeLaunchctl{}
	if code, out, errOut, _ := agentCLI(t, noProject, home, env, f, "uninstall"); code != 0 || out != "not installed\n" || errOut != "" {
		t.Fatalf("code %d, out %q, err %q", code, out, errOut)
	}
	if code, _, _, _ := agentCLI(t, noProject, home, env, f, "install"); code != 1 {
		t.Fatalf("install: code %d", code)
	}
	f.calls = nil
	code, out, errOut, plist := agentCLI(t, noProject, home, env, f, "uninstall")
	if code != 0 || out != "removed "+plist+"\n" || errOut != "" {
		t.Fatalf("code %d, out %q, err %q", code, out, errOut)
	}
	if want := []string{"print", "bootout"}; !slices.Equal(f.calls, want) || f.loaded {
		t.Fatalf("calls %q, want %q", f.calls, want)
	}
	if _, err := os.Stat(plist); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the plist is still there: %v", err)
	}
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
		if got.Namespace != "t1" || got.ConfigDir != env["SBX_BROKER_CONFIG_DIR"] || got.Editor == nil ||
			got.Getenv == nil || got.Getenv("SBX_BROKER_CONFIG_DIR") != env["SBX_BROKER_CONFIG_DIR"] {
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

// A bad marker does not block the TUI: it starts with no namespace and shows
// the marker error, which names the marker's path (escaped) and the rule,
// never the content.
func TestTUIStartsWithBadMarker(t *testing.T) {
	env, _ := broker(t)
	dir := filepath.Join(t.TempDir(), "evil\x1b]0;x\x07")
	writeFile(t, filepath.Join(dir, markerName), "Bad Name\n", 0o644)
	var got *tui.Options
	code, out, errOut := cliTUI(dir, env, func(_ tui.Client, opt tui.Options) error { got = &opt; return nil })
	if code != 0 || out != "" || errOut != "" || got == nil {
		t.Fatalf("code %d, out %q, err %q, ran %v", code, out, errOut, got != nil)
	}
	marker := eventlog.Printable(filepath.Join(dir, markerName))
	if got.Namespace != "" || !strings.HasPrefix(got.Note, "marker: "+marker+": ") ||
		!strings.Contains(got.Note, "not a valid namespace name (want 1-16 characters of a-z, 0-9)") ||
		strings.Contains(got.Note, "Bad Name") || strings.ContainsAny(got.Note, "\x1b\x07") {
		t.Fatalf("namespace %q, note %q", got.Namespace, got.Note)
	}
}

// With a bad marker and no broker, the no-broker line wins.
func TestTUIBadMarkerAndNoBroker(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, markerName), "Bad Name\n", 0o644)
	ran := false
	code, out, errOut := cliTUI(dir, map[string]string{"SBX_BROKER_STATE_DIR": stateDir(t)},
		func(tui.Client, tui.Options) error { ran = true; return nil })
	if code != 1 || out != "" || errOut != noBroker+"\n" || ran {
		t.Fatalf("code %d, out %q, err %q, ran %v", code, out, errOut, ran)
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

// A broker that accepts and never answers: the TUI does not start on a blank
// terminal; it prints one line and exits 1.
func TestTUIStartSilentBroker(t *testing.T) {
	state := stateDir(t)
	silent(t, state)
	ran := false
	var code int
	var out, errOut string
	within(t, 3*time.Second, "the TUI start", func() {
		code, out, errOut = cliTUI(noProject, map[string]string{"SBX_BROKER_STATE_DIR": state},
			func(tui.Client, tui.Options) error { ran = true; return nil })
	})
	if code != 1 || out != "" || errOut != "sbx-broker: the broker does not answer\n" || ran {
		t.Fatalf("code %d, out %q, err %q, ran %v", code, out, errOut, ran)
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
	for _, args := range [][]string{{"nope"}, {"refresh"}, {"status", "a", "b"}, {"reload", "x"}, {"log", "-x"}, {"serve", "x"}, {"tui", "x"}, {"install", "x"}, {"uninstall", "x"}} {
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

// eventlog.TestLogLineIsShared pins the line format; this pins that the CLI
// prints exactly that line, one per event.
func TestPrintEventLine(t *testing.T) {
	at := time.Date(2026, 11, 22, 13, 2, 11, 0, time.Local)
	for _, e := range []eventlog.Event{
		{Time: at, Kind: eventlog.KindSwap, Namespace: "t1", Route: "api", Method: "GET", Path: "/v1",
			Status: 200, Secrets: []string{"api-key", "other"}},
		{Time: at, Kind: eventlog.KindSwap, Namespace: "t1", Route: "app", User: "app",
			Secrets: []string{"db-pass"}, Message: "login"},
	} {
		var b bytes.Buffer
		if err := printEvent(&b, e); err != nil {
			t.Fatal(err)
		}
		if want := eventlog.Line(e) + "\n"; b.String() != want {
			t.Fatalf("got %q, want %q", b.String(), want)
		}
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
