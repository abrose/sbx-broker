package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/abrose/sbx-broker/internal/admin"
	"github.com/abrose/sbx-broker/internal/eventlog"
	"github.com/abrose/sbx-broker/internal/secrets"
	"github.com/abrose/sbx-broker/internal/server"
)

const fakeValue = "real-api-key-1"

// fakeClient answers from memory and records the calls.
type fakeClient struct {
	mu        sync.Mutex
	nss       []admin.NamespaceStatus
	statusErr error
	revealErr error
	refreshes []string // "ns/secret"
	reveals   []string // "ns/secret"
	values    [][]byte // every slice Reveal returned, to check the wipe
	statuses  int
	script    []followStep // one per Follow call; later calls just wait
	follows   int
	reloads   int
}

func (f *fakeClient) Status(ns string) ([]admin.NamespaceStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses++
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	return f.nss, nil
}

func (f *fakeClient) Refresh(ns, secret string) ([]admin.NamespaceStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshes = append(f.refreshes, ns+"/"+secret)
	for _, n := range f.nss {
		if n.Name == ns {
			return []admin.NamespaceStatus{n}, nil
		}
	}
	return nil, errors.New("unknown namespace " + ns)
}

func (f *fakeClient) Reload() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reloads++
	return nil
}

func (f *fakeClient) Reveal(ns, secret string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reveals = append(f.reveals, ns+"/"+secret)
	if f.revealErr != nil {
		return nil, f.revealErr
	}
	v := []byte(fakeValue)
	f.values = append(f.values, v)
	return v, nil
}

// followStep is what one Follow call does: send events, then fail with err,
// or (err nil) wait for the context.
type followStep struct {
	events []eventlog.Event
	err    error
}

func (f *fakeClient) Follow(ctx context.Context, each func(eventlog.Event)) error {
	f.mu.Lock()
	f.follows++
	var step followStep
	if len(f.script) > 0 {
		step, f.script = f.script[0], f.script[1:]
	}
	f.mu.Unlock()
	for _, e := range step.events {
		each(e)
	}
	if step.err != nil {
		return step.err
	}
	<-ctx.Done()
	return nil
}

func (f *fakeClient) followCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.follows
}

var fetched = time.Date(2026, 11, 23, 15, 4, 5, 0, time.Local)

func fixture() []admin.NamespaceStatus {
	return []admin.NamespaceStatus{{
		Name:      "acme",
		Listeners: []string{"127.0.0.1:29610"},
		Secrets: []secrets.Status{
			{Name: "api-key", Placeholder: "sbxb_acme_api_key", State: secrets.StateOK, FetchedAt: fetched,
				Length: 14, Fingerprint: "1a2b3c4d", Command: []string{"op", "read", "op://<vault>/<item>/credential"}, Swaps: 3},
			{Name: "db-pass", Placeholder: "sbxb_acme_db_pass", State: secrets.StateFailed, ExitCode: 1,
				Err: "no such entry\x1b[2J", Command: []string{"gopass", "show", "-o", "<path>"}},
		},
		HTTP: []admin.RouteInfo{{Name: "api", Listen: 29610, Upstream: "https://api.example.com", Transforms: []string{"swap"}}},
		Postgres: &admin.PostgresInfo{Listen: 29620, Databases: []admin.DatabaseInfo{
			{Alias: "main", Upstream: "127.0.0.1:5432", DBName: "appdb", User: "appuser", Password: "db-pass"},
		}},
	}, {
		Name:      "beta",
		Listeners: []string{},
		Secrets:   []secrets.Status{},
		Error:     "route api: address already in use",
	}}
}

// harness drives a Model as the Bubble Tea runtime would, but runs every
// command at once in the test goroutine. Timers are recorded, not run.
type harness struct {
	t      *testing.T
	m      *Model
	fake   *fakeClient
	timers []timer
	quit   bool
}

type timer struct {
	d   time.Duration
	msg tea.Msg
}

func newHarness(t *testing.T, cl Client, opt Options) *harness {
	t.Helper()
	h := &harness{t: t, m: New(cl, opt)}
	if f, ok := cl.(*fakeClient); ok {
		h.fake = f
	}
	h.m.after = func(d time.Duration, msg tea.Msg) tea.Cmd {
		h.timers = append(h.timers, timer{d, msg})
		return nil
	}
	// The editor runs at once, as Bubble Tea would run it between two frames.
	h.m.exec = func(c tea.ExecCommand, fn tea.ExecCallback) tea.Cmd {
		return func() tea.Msg { return fn(c.Run()) }
	}
	t.Cleanup(h.m.stopFollow)
	h.run(h.m.Init())
	return h
}

func start(t *testing.T, opt Options) *harness {
	t.Helper()
	return newHarness(t, &fakeClient{nss: fixture()}, opt)
}

func (h *harness) send(msg tea.Msg) {
	_, cmd := h.m.Update(msg)
	h.run(cmd)
}

func (h *harness) run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range msg {
			h.run(c)
		}
	case tea.QuitMsg:
		h.quit = true
	default:
		h.send(msg)
	}
}

func (h *harness) keys(keys ...string) {
	for _, k := range keys {
		h.send(keyPress(k))
	}
}

func keyPress(k string) tea.KeyPressMsg {
	switch k {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "shift+tab":
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	case "ctrl+s":
		return tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl}
	case "ctrl+u":
		return tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl}
	case "ctrl+v":
		return tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	}
	r := []rune(k)
	return tea.KeyPressMsg{Code: r[0], Text: k}
}

// typeText types s one key at a time.
func (h *harness) typeText(s string) {
	for _, r := range s {
		h.send(keyPress(string(r)))
	}
}

func (h *harness) screen() string { return h.m.render() }

func (h *harness) has(parts ...string) {
	h.t.Helper()
	s := h.screen()
	for _, p := range parts {
		if !strings.Contains(s, p) {
			h.t.Fatalf("screen has no %q:\n%s", p, s)
		}
	}
}

func (h *harness) hasNot(parts ...string) {
	h.t.Helper()
	s := h.screen()
	for _, p := range parts {
		if strings.Contains(s, p) {
			h.t.Fatalf("screen has %q:\n%s", p, s)
		}
	}
}

// revealed opens Inspect of api-key in acme and reveals it.
func revealed(t *testing.T) *harness {
	t.Helper()
	h := start(t, Options{Namespace: "acme"})
	h.keys("enter", "r", "y")
	h.has("value: " + fakeValue + "   (any key hides it)")
	return h
}

func zero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

func (h *harness) wiped() {
	h.t.Helper()
	h.hasNot(fakeValue)
	if h.m.value != nil {
		h.t.Fatal("the model still holds the value")
	}
	if len(h.fake.values) == 0 {
		h.t.Fatal("no value was revealed")
	}
	for _, v := range h.fake.values {
		if len(v) == 0 || !zero(v) {
			h.t.Fatalf("value not wiped: %q", v)
		}
	}
}

func TestStartsOnMarkerNamespace(t *testing.T) {
	h := start(t, Options{Namespace: "acme"})
	h.has("Secrets of acme", "sbxb_acme_api_key", "sbxb_acme_db_pass")
	h.hasNot("LISTENERS")
}

func TestStartsOnNamespacesWithoutMarker(t *testing.T) {
	h := start(t, Options{})
	h.has("LISTENERS", "acme", "beta")
}

func TestNamespacesView(t *testing.T) {
	h := start(t, Options{})
	h.has("NAME", "LISTENERS", "SECRETS", "ERROR", "acme", "29610", "1 of 2 ok", "0 of 0 ok", "route api: address already in use")
	// enter on the second row opens the Secrets of beta.
	h.keys("j", "enter")
	h.has("Secrets of beta")
	h.keys("1", "down", "up", "enter")
	h.has("Secrets of acme")
}

func TestSecretsView(t *testing.T) {
	h := start(t, Options{Namespace: "acme"})
	h.has("NAME", "PLACEHOLDER", "COMMAND", "STATE", "FETCHED", "SWAPS",
		"sbxb_acme_api_key", "op read 'op://<vault>/<item>/credential'", "ok", "15:04:05", " 3",
		"gopass show -o '<path>'", "failed")
	// The failed row has no time.
	for _, line := range strings.Split(h.screen(), "\n") {
		if strings.Contains(line, "db-pass") && !strings.Contains(line, " - ") {
			t.Fatalf("no - for FETCHED in %q", line)
		}
	}
}

func TestSecretsViewCutsCommand(t *testing.T) {
	h := start(t, Options{Namespace: "acme"})
	h.send(tea.WindowSizeMsg{Width: 70, Height: 24})
	h.hasNot("op read 'op://<vault>/<item>/credential'")
	h.has("op read", "…")
	for _, line := range strings.Split(h.screen(), "\n") {
		if strings.Contains(line, "api-key") && len([]rune(line)) > 70 {
			t.Fatalf("line wider than 70: %q", line)
		}
	}
}

func TestBrokerTextIsEscaped(t *testing.T) {
	h := start(t, Options{Namespace: "acme"})
	h.keys("j", "enter")
	h.has(`no such entry\x1b[2J`, "exit code", "1")
	if strings.Contains(h.screen(), "\x1b[2J") {
		t.Fatal("a control sequence from the broker reaches the screen")
	}
}

func TestRefreshKeys(t *testing.T) {
	h := start(t, Options{Namespace: "acme"})
	h.keys("f", "F")
	want := []string{"acme/api-key", "acme/"}
	if strings.Join(h.fake.refreshes, ",") != strings.Join(want, ",") {
		t.Fatalf("refreshes %q, want %q", h.fake.refreshes, want)
	}
}

func TestRefreshShowsFetching(t *testing.T) {
	h := start(t, Options{Namespace: "acme"})
	_, cmd := h.m.Update(keyPress("f"))
	h.has("fetching")
	h.run(cmd)
	h.hasNot("fetching")
}

func TestInspectShowsFingerprintNotValue(t *testing.T) {
	h := start(t, Options{Namespace: "acme"})
	h.keys("enter")
	h.has("api-key", "sbxb_acme_api_key", "op read 'op://<vault>/<item>/credential'", "14", "sha256 1a2b3c4d",
		"swaps", "value: hidden (r to reveal)")
	h.hasNot(fakeValue)
	if len(h.fake.reveals) != 0 {
		t.Fatalf("reveals %q", h.fake.reveals)
	}
	h.keys("esc")
	h.has("Secrets of acme")
}

func TestRevealNeedsConfirmation(t *testing.T) {
	h := start(t, Options{Namespace: "acme"})
	h.keys("enter", "r")
	h.has("Reveal the value of api-key? The broker logs it. (y/n)")
	h.keys("n")
	h.has("value: hidden (r to reveal)")
	if len(h.fake.reveals) != 0 {
		t.Fatalf("reveals %q", h.fake.reveals)
	}
	h.keys("r", "y")
	if len(h.fake.reveals) != 1 || h.fake.reveals[0] != "acme/api-key" {
		t.Fatalf("reveals %q", h.fake.reveals)
	}
	h.has("value: " + fakeValue + "   (any key hides it)")
}

func TestRevealHidesOnNextKeyAndWipes(t *testing.T) {
	h := revealed(t)
	h.keys("x")
	h.wiped()
	h.has("value: hidden (r to reveal)")
}

func TestRevealHidesOnViewSwitch(t *testing.T) {
	h := revealed(t)
	h.keys("4")
	h.wiped()
	if h.m.view != viewLog {
		t.Fatalf("view %v, want the Log view", h.m.view)
	}
}

func TestRevealHidesAfter30s(t *testing.T) {
	h := revealed(t)
	first := h.lastTimer(30 * time.Second)
	h.keys("x", "r", "y")
	second := h.lastTimer(30 * time.Second)
	h.send(first.msg)
	h.has("value: " + fakeValue)
	h.send(second.msg)
	h.wiped()
	h.has("value: hidden (r to reveal)")
}

func (h *harness) lastTimer(d time.Duration) timer {
	h.t.Helper()
	for i := len(h.timers) - 1; i >= 0; i-- {
		if h.timers[i].d == d {
			return h.timers[i]
		}
	}
	h.t.Fatalf("no timer of %v in %v", d, h.timers)
	return timer{}
}

func TestRevealKeptOnStatusPoll(t *testing.T) {
	h := revealed(t)
	before := h.fake.statuses
	h.send(h.lastTimer(2 * time.Second).msg)
	if h.fake.statuses != before+1 {
		t.Fatalf("the poll sent %d status requests", h.fake.statuses-before)
	}
	h.has("value: " + fakeValue)
	if string(h.fake.values[0]) != fakeValue {
		t.Fatalf("bytes changed: %q", h.fake.values[0])
	}
}

// A status without the shown secret (its definition was removed) hides the
// value: the screen could no longer show it.
func TestRevealHidesWhenSecretGoes(t *testing.T) {
	h := revealed(t)
	nss := fixture()
	nss[0].Secrets = nss[0].Secrets[1:]
	h.fake.nss = nss
	h.send(h.lastTimer(2 * time.Second).msg)
	h.wiped()
}

func TestBeforeFirstStatus(t *testing.T) {
	for _, opt := range []Options{{}, {Namespace: "acme"}} {
		m := New(&fakeClient{nss: fixture()}, opt)
		if s := m.render(); !strings.Contains(s, "asking the broker") || strings.Contains(s, "no namespace") {
			t.Fatalf("%+v: %s", opt, s)
		}
	}
}

func TestRevealErrorShown(t *testing.T) {
	h := start(t, Options{Namespace: "acme"})
	h.fake.revealErr = errors.New("secret not loaded: db-pass (failed)\x1b[2J")
	h.keys("j", "enter", "r", "y")
	h.has(`secret not loaded: db-pass (failed)\x1b[2J`)
	h.hasNot("value: hidden", "\x1b[2J")
	h.keys("x")
	h.has("value: hidden (r to reveal)")
}

// A value that arrives after the owner left the reveal is wiped at once.
func TestLateRevealWiped(t *testing.T) {
	h := start(t, Options{Namespace: "acme"})
	h.keys("enter", "r")
	_, cmd := h.m.Update(keyPress("y"))
	h.keys("esc")
	h.run(cmd)
	h.wiped()
	if len(h.fake.values) != 1 {
		t.Fatalf("values %d", len(h.fake.values))
	}
}

func TestBrokerGoneAndBack(t *testing.T) {
	h := start(t, Options{})
	h.fake.statusErr = admin.ErrNoBroker
	h.send(h.lastTimer(2 * time.Second).msg)
	h.has(NoBroker)
	if h.quit {
		t.Fatal("the TUI quit")
	}
	h.fake.statusErr = nil
	h.send(h.lastTimer(2 * time.Second).msg)
	h.hasNot(NoBroker)
	h.has("acme", "1 of 2 ok")
}

func TestQuitWipesShownValue(t *testing.T) {
	h := revealed(t)
	h.keys("q")
	if !h.quit {
		t.Fatal("q did not quit")
	}
	h.wiped()
}

func TestCtrlCQuits(t *testing.T) {
	h := revealed(t)
	h.keys("ctrl+c")
	if !h.quit {
		t.Fatal("ctrl+c did not quit")
	}
	h.wiped()
}

func TestTabCyclesViews(t *testing.T) {
	h := start(t, Options{})
	var seen []view
	for range 4 {
		h.keys("tab")
		seen = append(seen, h.m.view)
	}
	want := []view{viewSecrets, viewRoutes, viewLog, viewNamespaces}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("tab gave %v, want %v", seen, want)
		}
	}
}

func TestFooterShowsKeysOfView(t *testing.T) {
	h := start(t, Options{Namespace: "acme"})
	h.has("refresh")
	h.keys("enter")
	h.has("reveal", "back")
	h.hasNot("refresh all")
}

// TestTUIAgainstRealBroker drives the model against a real broker over the
// admin socket: the agent's stand-in for trying the TUI by hand.
func TestTUIAgainstRealBroker(t *testing.T) {
	_, cl := realBroker(t, "secrets:\n  api-key:\n    command: [\"printf\", \"%s\", \"real-api-key-1\"]\n")

	h := newHarness(t, cl, Options{})
	h.has("t1", "1 of 1 ok")
	h.keys("enter")
	h.has("Secrets of t1", "sbxb_t1_api_key", "printf %s real-api-key-1")
	h.keys("enter")
	h.has("sha256 ", "value: hidden (r to reveal)")
	h.hasNot("value: real-api-key-1")
	h.keys("r", "y")
	h.has("value: real-api-key-1   (any key hides it)")
	value := h.m.value
	h.keys("j")
	h.hasNot("value: real-api-key-1")
	if h.m.value != nil || len(value) != len("real-api-key-1") || !zero(value) {
		t.Fatalf("value not wiped: %q", value)
	}

	// The reveal is in the broker's log.
	fctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	var got *eventlog.Event
	err := cl.Follow(fctx, func(e eventlog.Event) {
		if e.Kind == eventlog.KindReveal {
			got = &e
			stop()
		}
	})
	if got == nil || got.Namespace != "t1" || strings.Join(got.Secrets, ",") != "api-key" || got.Message != "revealed" {
		t.Fatalf("no reveal event: %+v, %v", got, err)
	}
}

// realBroker runs a broker in-process on a config dir (0700) with the
// namespace file t1.yaml holding file, and waits until t1 has secrets and
// each of them is ok. It returns the config dir and a socket client.
func realBroker(t *testing.T, file string) (string, Client) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "namespaces")
	if err := os.Mkdir(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "t1.yaml"), []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := os.MkdirTemp("/tmp", "sbxb") // a socket path has at most 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = server.Run(ctx, server.Options{ConfigDir: cfg, StateDir: state, FetchTimeout: 10 * time.Second, PollInterval: 100 * time.Millisecond})
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done })
	cl := NewSocketClient(state)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		nss, err := cl.Status("")
		if err == nil && len(nss) == 1 && len(nss[0].Secrets) > 0 && allOK(nss[0].Secrets) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the broker did not get ready: %v %v", nss, err)
		}
	}
	return cfg, cl
}

func allOK(ss []secrets.Status) bool {
	for _, s := range ss {
		if s.State != secrets.StateOK {
			return false
		}
	}
	return true
}

func TestSocketClientNoBroker(t *testing.T) {
	state, err := os.MkdirTemp("/tmp", "sbxb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	if _, err := NewSocketClient(state).Status(""); !errors.Is(err, admin.ErrNoBroker) {
		t.Fatalf("err %v", err)
	}
}

func TestViewUsesAltScreen(t *testing.T) {
	h := start(t, Options{})
	if v := h.m.View(); !v.AltScreen || !strings.Contains(v.Content, "acme") {
		t.Fatalf("view %+v", v)
	}
}
