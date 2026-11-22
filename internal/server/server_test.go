package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abrose/sbx-broker/internal/admin"
	"github.com/abrose/sbx-broker/internal/eventlog"
	"github.com/abrose/sbx-broker/internal/secrets"
)

const (
	apiKey   = "real-api-key-1"
	otherKey = "other-key-value-22"
	newKey   = "new-api-key-value-33"
)

// allValues are every test value; no log line may hold one.
var allValues = []string{apiKey, otherKey, newKey}

// upstream records the X-Api-Key header of the last request.
type upstream struct {
	*httptest.Server
	mu   sync.Mutex
	last string
	gate chan struct{} // when set, each request waits for it to close
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.last = r.Header.Get("X-Api-Key")
		gate := u.gate
		u.mu.Unlock()
		if gate != nil {
			<-gate
		}
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) lastKey() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.last
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// dirs returns a config dir (0700) and a short state dir (ruling R5).
func dirs(t *testing.T) (cfg, state string) {
	t.Helper()
	cfg = filepath.Join(t.TempDir(), "namespaces")
	if err := os.Mkdir(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	state, err := os.MkdirTemp("/tmp", "sbxb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	return cfg, state
}

// writeNS writes a namespace file atomically (a temp file, then a rename),
// so a poll never sees it half written.
func writeNS(t *testing.T, dir, ns, content string) {
	t.Helper()
	tmp := filepath.Join(dir, "."+ns+".tmp")
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, ns+".yaml")); err != nil {
		t.Fatal(err)
	}
}

func secretYAML(name string, cmd ...string) string {
	q := make([]string, len(cmd))
	for i, c := range cmd {
		q[i] = fmt.Sprintf("%q", c)
	}
	return fmt.Sprintf("  %s:\n    command: [%s]\n", name, strings.Join(q, ", "))
}

func routeYAML(name string, port int, up string) string {
	return fmt.Sprintf("  - name: %s\n    listen: %d\n    upstream: %s\n", name, port, up)
}

// nsYAML builds a namespace file of secrets and routes.
func nsYAML(secretLines, routeLines []string) string {
	var b strings.Builder
	if len(secretLines) > 0 {
		b.WriteString("secrets:\n")
		for _, s := range secretLines {
			b.WriteString(s)
		}
	}
	if len(routeLines) > 0 {
		b.WriteString("http:\n")
		for _, r := range routeLines {
			b.WriteString(r)
		}
	}
	return b.String()
}

type run struct {
	cancel context.CancelFunc
	done   chan struct{} // closed when Run returned
	err    error
}

// start runs the broker until the test ends, and waits for its first
// answer. The socket accepts before the first scan ends; it answers after.
func start(t *testing.T, cfg, state string) *run {
	t.Helper()
	r := startNoWait(t, cfg, state)
	waitFor(t, 3*time.Second, "the admin socket", func() bool {
		_, err := do(t, state, admin.Request{Op: "status"})
		return err == nil
	})
	return r
}

func startNoWait(t *testing.T, cfg, state string) *run {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &run{cancel: cancel, done: make(chan struct{})}
	go func() {
		r.err = Run(ctx, Options{ConfigDir: cfg, StateDir: state, FetchTimeout: 10 * time.Second, PollInterval: 100 * time.Millisecond})
		close(r.done)
	}()
	t.Cleanup(func() {
		if err := r.stop(); err != nil {
			t.Error(err)
		}
	})
	return r
}

// stop ends Run and returns its error. It may run more than once and from
// any goroutine.
func (r *run) stop() error {
	r.cancel()
	select {
	case <-r.done:
		return r.err
	case <-time.After(15 * time.Second):
		return errors.New("Run did not end")
	}
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func do(t *testing.T, state string, req admin.Request) ([]admin.Response, error) {
	t.Helper()
	c, err := admin.Dial(state)
	if err != nil {
		return nil, err
	}
	var out []admin.Response
	err = c.Do(req, func(r admin.Response) error {
		out = append(out, r)
		return nil
	})
	return out, err
}

func status(t *testing.T, state, ns string) (admin.NamespaceStatus, bool) {
	t.Helper()
	resps, err := do(t, state, admin.Request{Op: "status"})
	if err != nil || len(resps) != 1 {
		return admin.NamespaceStatus{}, false
	}
	for _, n := range resps[0].Namespaces {
		if n.Name == ns {
			return n, true
		}
	}
	return admin.NamespaceStatus{}, false
}

func secretState(t *testing.T, state, ns, secret string) secrets.State {
	t.Helper()
	n, ok := status(t, state, ns)
	if !ok {
		return ""
	}
	for _, s := range n.Secrets {
		if s.Name == secret {
			return s.State
		}
	}
	return ""
}

func waitOK(t *testing.T, state, ns string, names ...string) {
	t.Helper()
	for _, name := range names {
		waitFor(t, 5*time.Second, "secret "+name+" ok", func() bool { return secretState(t, state, ns, name) == secrets.StateOK })
	}
}

var client = &http.Client{Transport: &http.Transport{DisableKeepAlives: true, Proxy: nil}, Timeout: 5 * time.Second}

// get sends a request with the placeholder in X-Api-Key to the port.
func get(port int, path, ph string) (int, string, error) {
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("X-Api-Key", ph)
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), err
}

func readLog(t *testing.T, state string) []eventlog.Event {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(state, "broker.log"))
	if err != nil {
		t.Fatal(err)
	}
	var out []eventlog.Event
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var e eventlog.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func hasEvent(events []eventlog.Event, f func(eventlog.Event) bool) bool {
	for _, e := range events {
		if f(e) {
			return true
		}
	}
	return false
}

// noValues fails when the log files hold any test value, raw or in the
// JSON form the file has (json.Marshal writes & < > as & ...). It
// names none.
func noValues(t *testing.T, state string, values ...string) {
	t.Helper()
	var forms []string
	for _, v := range values {
		j, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		forms = append(forms, v, strings.Trim(string(j), `"`))
	}
	noForms(t, state, forms...)
}

func noForms(t *testing.T, state string, values ...string) {
	t.Helper()
	for _, name := range []string{"broker.log", "broker.log.1"} {
		data, err := os.ReadFile(filepath.Join(state, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for i, v := range values {
			if strings.Contains(string(data), v) {
				t.Errorf("%s holds test value #%d", name, i)
			}
		}
	}
}

func TestServeStatus(t *testing.T) {
	t.Parallel()
	cfg, state := dirs(t)
	up := newUpstream(t)
	port := freePort(t)
	writeNS(t, cfg, "t1", nsYAML(
		[]string{secretYAML("api-key", "printf", "%s", apiKey)},
		[]string{routeYAML("api", port, up.URL)}))
	start(t, cfg, state)
	waitOK(t, state, "t1", "api-key")
	n, _ := status(t, state, "t1")
	if len(n.Listeners) != 1 || n.Listeners[0] != fmt.Sprintf("127.0.0.1:%d", port) {
		t.Fatalf("listeners %v", n.Listeners)
	}
	if len(n.Secrets) != 1 || n.Secrets[0].Placeholder != "sbxb_t1_api_key" || n.Secrets[0].Length != len(apiKey) {
		t.Fatalf("secrets %+v", n.Secrets)
	}
}

func TestServeOpensListenersBeforeFetchEnds(t *testing.T) {
	t.Parallel()
	cfg, state := dirs(t)
	up := newUpstream(t)
	port := freePort(t)
	writeNS(t, cfg, "t1", nsYAML(
		[]string{secretYAML("api-key", "printf", "%s", apiKey), secretYAML("slow", "sleep", "3")},
		[]string{routeYAML("api", port, up.URL)}))
	began := time.Now()
	start(t, cfg, state)
	waitFor(t, time.Second, "the quick secret", func() bool {
		code, _, err := get(port, "/v1", "sbxb_t1_api_key")
		return err == nil && code == http.StatusOK
	})
	if up.lastKey() != apiKey {
		t.Fatal("upstream did not get the value")
	}
	code, body, err := get(port, "/v1", "sbxb_t1_slow")
	if err != nil || code != http.StatusServiceUnavailable || !strings.Contains(body, `"state":"pending"`) {
		t.Fatalf("slow: %d %s %v", code, body, err)
	}
	if d := time.Since(began); d > 1500*time.Millisecond {
		t.Fatalf("took %s", d)
	}
}

func TestServeSecondInstance(t *testing.T) {
	t.Parallel()
	cfg, state := dirs(t)
	start(t, cfg, state)
	err := Run(context.Background(), Options{ConfigDir: cfg, StateDir: state, PollInterval: 100 * time.Millisecond})
	if err == nil || err.Error() != "a broker already runs" {
		t.Fatalf("got %v", err)
	}
	// The first broker still answers.
	if _, err := do(t, state, admin.Request{Op: "status"}); err != nil {
		t.Fatal(err)
	}
}

// A socket file left without the lock is stale; Run replaces it (no probe,
// so no dial races the forking tests).
func TestServeRemovesStaleSocket(t *testing.T) {
	t.Parallel()
	cfg, state := dirs(t)
	ln, err := net.Listen("unix", filepath.Join(state, "admin.sock"))
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = ln.Close()
	start(t, cfg, state)
}

func TestServeReloadKeepsOldOnError(t *testing.T) {
	t.Parallel()
	cfg, state := dirs(t)
	up := newUpstream(t)
	port := freePort(t)
	writeNS(t, cfg, "t1", nsYAML(
		[]string{secretYAML("api-key", "printf", "%s", apiKey)},
		[]string{routeYAML("api", port, up.URL)}))
	start(t, cfg, state)
	waitOK(t, state, "t1", "api-key")
	writeNS(t, cfg, "t1", "secrets: [this is not\n")
	waitFor(t, 2*time.Second, "an error event", func() bool {
		return hasEvent(readLog(t, state), func(e eventlog.Event) bool {
			return e.Kind == eventlog.KindError && strings.Contains(e.Message, "t1.yaml")
		})
	})
	code, _, err := get(port, "/v1", "sbxb_t1_api_key")
	if err != nil || code != http.StatusOK {
		t.Fatalf("old route: %d %v", code, err)
	}
}

func TestServeReloadPicksUpChanges(t *testing.T) {
	t.Parallel()
	cfg, state := dirs(t)
	up := newUpstream(t)
	port := freePort(t)
	route := []string{routeYAML("api", port, up.URL)}
	writeNS(t, cfg, "t1", nsYAML([]string{secretYAML("api-key", "printf", "%s", apiKey)}, route))
	start(t, cfg, state)
	waitOK(t, state, "t1", "api-key")
	writeNS(t, cfg, "t1", nsYAML([]string{secretYAML("api-key", "printf", "%s", newKey)}, route))
	waitFor(t, 2*time.Second, "the new value upstream", func() bool {
		code, _, err := get(port, "/v1", "sbxb_t1_api_key")
		return err == nil && code == http.StatusOK && up.lastKey() == newKey
	})
}

func TestServeReloadOp(t *testing.T) {
	t.Parallel()
	cfg, state := dirs(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		// A poll interval far longer than the test: only the reload op scans.
		done <- Run(ctx, Options{ConfigDir: cfg, StateDir: state, FetchTimeout: 10 * time.Second, PollInterval: time.Hour})
	}()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, 3*time.Second, "the admin socket", func() bool { _, err := do(t, state, admin.Request{Op: "status"}); return err == nil })
	writeNS(t, cfg, "t2", nsYAML([]string{secretYAML("api-key", "printf", "%s", apiKey)}, nil))
	if _, err := do(t, state, admin.Request{Op: "reload"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := status(t, state, "t2"); !ok {
		t.Fatal("reload did not load t2")
	}
}

func TestServeRemovedFileStopsNamespace(t *testing.T) {
	t.Parallel()
	cfg, state := dirs(t)
	up := newUpstream(t)
	port := freePort(t)
	writeNS(t, cfg, "t1", nsYAML(
		[]string{secretYAML("api-key", "printf", "%s", apiKey), secretYAML("slow", "sleep", "30")},
		[]string{routeYAML("api", port, up.URL)}))
	start(t, cfg, state)
	waitOK(t, state, "t1", "api-key")
	if err := os.Remove(filepath.Join(cfg, "t1.yaml")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, "the port to refuse", func() bool {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
		}
		return err != nil
	})
	if _, ok := status(t, state, "t1"); ok {
		t.Fatal("t1 still in status")
	}
	// The running fetch of the removed namespace is cancelled.
	waitFor(t, 3*time.Second, "the cancelled fetch", func() bool {
		return hasEvent(readLog(t, state), func(e eventlog.Event) bool {
			return e.Kind == eventlog.KindFetch && e.Message == "failed: cancelled"
		})
	})
}

func TestServeLogHasNoValues(t *testing.T) {
	t.Parallel()
	cfg, state := dirs(t)
	up := newUpstream(t)
	port := freePort(t)
	writeNS(t, cfg, "t1", nsYAML(
		[]string{
			secretYAML("api-key", "printf", "%s", apiKey),
			secretYAML("other", "printf", "%s", otherKey),
			// A fetch tool that prints a value on stderr while it fails.
			secretYAML("broken", "sh", "-c", "echo "+newKey+" >&2; exit 3"),
		},
		[]string{routeYAML("api", port, up.URL)}))
	r := start(t, cfg, state)
	waitOK(t, state, "t1", "api-key", "other")
	for _, path := range []string{"/v1", "/v1/" + otherKey, "/v1/sbxb_t1_other", "/v1/sbxb_t1_broken"} {
		if _, _, err := get(port, path, "sbxb_t1_api_key"); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	events := readLog(t, state)
	if !hasEvent(events, func(e eventlog.Event) bool { return e.Kind == eventlog.KindSwap }) {
		t.Fatal("no swap event")
	}
	if !hasEvent(events, func(e eventlog.Event) bool { return e.Message == "failed: exit 3" }) {
		t.Fatal("no failed fetch event")
	}
	noValues(t, state, allValues...)
}

// A value that a refresh or a reload replaced stays in the redactor until
// the namespace stops: a late log line may still hold it.
func TestServeRedactsReplacedValues(t *testing.T) {
	t.Parallel()
	cfg, state := dirs(t)
	up := newUpstream(t)
	port := freePort(t)
	valueFile := filepath.Join(t.TempDir(), "value")
	if err := os.WriteFile(valueFile, []byte(apiKey), 0o600); err != nil {
		t.Fatal(err)
	}
	route := []string{routeYAML("api", port, up.URL)}
	writeNS(t, cfg, "t1", nsYAML([]string{secretYAML("api-key", "cat", valueFile)}, route))
	r := start(t, cfg, state)
	waitOK(t, state, "t1", "api-key")

	// A refresh replaces apiKey with otherKey.
	if err := os.WriteFile(valueFile, []byte(otherKey), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := do(t, state, admin.Request{Op: "refresh", Namespace: "t1", Secret: "api-key"}); err != nil {
		t.Fatal(err)
	}
	// A reload replaces otherKey with newKey.
	writeNS(t, cfg, "t1", nsYAML([]string{secretYAML("api-key", "printf", "%s", newKey)}, route))
	waitFor(t, 2*time.Second, "the new value upstream", func() bool {
		code, _, err := get(port, "/v1", "sbxb_t1_api_key")
		return err == nil && code == http.StatusOK && up.lastKey() == newKey
	})
	// The paths of these requests hold the old values in plain text.
	for _, v := range []string{apiKey, otherKey} {
		if _, _, err := get(port, "/old/"+v, "sbxb_t1_api_key"); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	noValues(t, state, allValues...)
}

// The proxy logs the escaped path, so the redactor must also know a value
// in that form.
func TestServeRedactsEscapedPath(t *testing.T) {
	t.Parallel()
	const pass = "p@ss word/1+&"
	cfg, state := dirs(t)
	up := newUpstream(t)
	port := freePort(t)
	writeNS(t, cfg, "t1", nsYAML(
		[]string{secretYAML("pass", "printf", "%s", pass)},
		[]string{routeYAML("api", port, up.URL)}))
	r := start(t, cfg, state)
	waitOK(t, state, "t1", "pass")
	if _, _, err := get(port, "/x/"+url.PathEscape(pass), ""); err != nil {
		t.Fatal(err)
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	if !hasEvent(readLog(t, state), func(e eventlog.Event) bool { return e.Kind == eventlog.KindSwap && strings.HasPrefix(e.Path, "/x/") }) {
		t.Fatal("no swap event")
	}
	noValues(t, state, pass, (&url.URL{Path: pass}).EscapedPath())
}

// pathEscape works on bytes (no string copy of a value) and must give what
// the proxy logs: url.URL.EscapedPath of the decoded path.
func TestPathEscapeMatchesURL(t *testing.T) {
	for c := range 256 {
		v := []byte{'a', byte(c), 'b'}
		if got, want := string(pathEscape(v)), (&url.URL{Path: string(v)}).EscapedPath(); got != want {
			t.Errorf("byte %#x: got %q, want %q", c, got, want)
		}
	}
}

func TestServeSocketMode(t *testing.T) {
	t.Parallel()
	cfg, base := dirs(t)
	state := filepath.Join(base, "st")
	start(t, cfg, state)
	assertModes(t, state)
}

// A state dir with a looser mode is tightened to 0700.
func TestServeFixesStateDirMode(t *testing.T) {
	t.Parallel()
	cfg, state := dirs(t)
	if err := os.Chmod(state, 0o755); err != nil {
		t.Fatal(err)
	}
	start(t, cfg, state)
	assertModes(t, state)
}

func assertModes(t *testing.T, state string) {
	t.Helper()
	fi, err := os.Stat(state)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("state dir mode %04o", fi.Mode().Perm())
	}
	fi, err = os.Stat(filepath.Join(state, "admin.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 || fi.Mode()&os.ModeSocket == 0 {
		t.Errorf("socket mode %v", fi.Mode())
	}
}

// An empty namespace file is a namespace with no secrets and no listeners.
func TestServeEmptyNamespace(t *testing.T) {
	t.Parallel()
	cfg, state := dirs(t)
	writeNS(t, cfg, "t1", "")
	start(t, cfg, state)
	n, ok := status(t, state, "t1")
	if !ok || len(n.Listeners) != 0 || len(n.Secrets) != 0 || n.Error != "" {
		t.Fatalf("status %+v %v", n, ok)
	}
}

// A listener that cannot start: the namespace keeps the others, status and
// the log show the error.
func TestServeListenerStartFails(t *testing.T) {
	t.Parallel()
	cfg, state := dirs(t)
	up := newUpstream(t)
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = taken.Close() })
	busy := taken.Addr().(*net.TCPAddr).Port
	port := freePort(t)
	writeNS(t, cfg, "t1", nsYAML(
		[]string{secretYAML("api-key", "printf", "%s", apiKey)},
		[]string{routeYAML("api", port, up.URL), routeYAML("idp", busy, up.URL)}))
	start(t, cfg, state)
	n, ok := status(t, state, "t1")
	if !ok || len(n.Listeners) != 1 || n.Listeners[0] != fmt.Sprintf("127.0.0.1:%d", port) {
		t.Fatalf("status %+v", n)
	}
	if !strings.Contains(n.Error, "idp") {
		t.Fatalf("error %q", n.Error)
	}
	if !hasEvent(readLog(t, state), func(e eventlog.Event) bool {
		return e.Kind == eventlog.KindError && e.Namespace == "t1" && strings.Contains(e.Message, "idp")
	}) {
		t.Fatal("no error event")
	}
}

func TestServePostgresInfo(t *testing.T) {
	t.Parallel()
	cfg, state := dirs(t)
	port := freePort(t)
	writeNS(t, cfg, "t1", nsYAML([]string{secretYAML("db-pass", "printf", "%s", apiKey)}, nil)+
		fmt.Sprintf("postgres:\n  listen: %d\n  databases:\n    app:\n      upstream: 127.0.0.1:5432\n      dbname: app\n      user: app\n      password: db-pass\n", port))
	start(t, cfg, state)
	if !hasEvent(readLog(t, state), func(e eventlog.Event) bool {
		return e.Kind == eventlog.KindInfo && e.Namespace == "t1" && e.Message == "postgres listeners come in a later version"
	}) {
		t.Fatal("no postgres info event")
	}
	if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond); err == nil {
		_ = c.Close()
		t.Fatal("a postgres port listens")
	}
}

// Run ends at once although a fetch still runs: the fetch is killed.
func TestServeStopKillsFetches(t *testing.T) {
	t.Parallel()
	cfg, state := dirs(t)
	writeNS(t, cfg, "t1", nsYAML([]string{secretYAML("slow", "sleep", "30")}, nil))
	r := start(t, cfg, state)
	began := time.Now()
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(began); d > 3*time.Second {
		t.Fatalf("Run took %s to end", d)
	}
	if _, err := os.Stat(filepath.Join(state, "admin.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("admin.sock still exists")
	}
}

// The listeners close before the log: a request that runs at shutdown
// still gets its swap event into broker.log.
func TestServeShutdownLogsOpenRequests(t *testing.T) {
	t.Parallel()
	cfg, state := dirs(t)
	up := newUpstream(t)
	up.gate = make(chan struct{})
	port := freePort(t)
	writeNS(t, cfg, "t1", nsYAML(
		[]string{secretYAML("api-key", "printf", "%s", apiKey)},
		[]string{routeYAML("api", port, up.URL)}))
	r := start(t, cfg, state)
	waitOK(t, state, "t1", "api-key")
	got := make(chan int, 1)
	go func() {
		code, _, _ := get(port, "/slow", "sbxb_t1_api_key")
		got <- code
	}()
	waitFor(t, 2*time.Second, "the request upstream", func() bool { return up.lastKey() != "" })
	stopped := make(chan error, 1)
	go func() { stopped <- r.stop() }()
	time.Sleep(200 * time.Millisecond)
	close(up.gate)
	if code := <-got; code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if !hasEvent(readLog(t, state), func(e eventlog.Event) bool { return e.Kind == eventlog.KindSwap && e.Path == "/slow" }) {
		t.Fatal("no swap event for the open request")
	}
}

func TestServeAdminErrors(t *testing.T) {
	t.Parallel()
	cfg, state := dirs(t)
	writeNS(t, cfg, "t1", nsYAML([]string{secretYAML("api-key", "printf", "%s", apiKey)}, nil))
	start(t, cfg, state)
	for _, tc := range []struct {
		req  admin.Request
		want string
	}{
		{admin.Request{Op: "nope"}, "unknown op nope"},
		{admin.Request{Op: "refresh", Namespace: "t9"}, "unknown namespace t9"},
		{admin.Request{Op: "refresh", Namespace: "t1", Secret: "nope"}, "unknown secret nope in namespace t1"},
		{admin.Request{Op: "status", Namespace: "t9"}, "unknown namespace t9"},
	} {
		_, err := do(t, state, tc.req)
		if err == nil || err.Error() != tc.want {
			t.Errorf("%+v: got %v", tc.req, err)
		}
	}
}
