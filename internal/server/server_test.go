package server

import (
	"bytes"
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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abrose/sbx-broker/internal/admin"
	"github.com/abrose/sbx-broker/internal/config"
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

// storingUpstream is a CRUD-like upstream: /store keeps the X-Api-Key
// header of the request, /show answers with what it kept.
func storingUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	var kept string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/store" {
			kept = r.Header.Get("X-Api-Key")
		}
		_, _ = io.WriteString(w, "kept: "+kept)
	}))
	t.Cleanup(s.Close)
	return s
}

// storeThenShow has the upstream keep the value of api-key, runs change
// (which takes the value out of the ok state), and returns what /show gives
// the app afterwards.
func storeThenShow(t *testing.T, port int, change func()) string {
	t.Helper()
	code, body, err := get(port, "/store", "sbxb_t1_api_key")
	if err != nil || code != http.StatusOK || body != "kept: sbxb_t1_api_key" {
		t.Fatalf("store: %d %v, body scrubbed: %v", code, err, body == "kept: sbxb_t1_api_key")
	}
	change()
	code, body, err = get(port, "/show", "")
	if err != nil || code != http.StatusOK {
		t.Fatalf("show: %d %v", code, err)
	}
	return body
}

// checkShown fails when the body the app got holds a test value.
func checkShown(t *testing.T, body string) {
	t.Helper()
	for i, v := range allValues {
		if strings.Contains(body, v) {
			t.Errorf("the app got test value #%d", i)
		}
	}
	if body != "kept: sbxb_t1_api_key" {
		t.Errorf("the stored value was not scrubbed to its placeholder")
	}
}

// A value that a failed refresh dropped is still scrubbed from the answers
// (finding C1): the upstream may have stored it.
func TestServeScrubsValueAfterFailedRefresh(t *testing.T) {
	t.Parallel()
	cfg, state := dirs(t)
	up := storingUpstream(t)
	port := freePort(t)
	valueFile := filepath.Join(t.TempDir(), "value")
	if err := os.WriteFile(valueFile, []byte(apiKey), 0o600); err != nil {
		t.Fatal(err)
	}
	writeNS(t, cfg, "t1", nsYAML([]string{secretYAML("api-key", "cat", valueFile)}, []string{routeYAML("api", port, up.URL)}))
	start(t, cfg, state)
	waitOK(t, state, "t1", "api-key")
	body := storeThenShow(t, port, func() {
		if err := os.Remove(valueFile); err != nil {
			t.Fatal(err)
		}
		if _, err := do(t, state, admin.Request{Op: "refresh", Namespace: "t1", Secret: "api-key"}); err != nil {
			t.Fatal(err)
		}
		if st := secretState(t, state, "t1", "api-key"); st != secrets.StateFailed {
			t.Fatalf("state %s after the failed refresh", st)
		}
	})
	checkShown(t, body)
}

// A reload that rewrites the namespace file starts a new store with every
// secret pending; the old value is still scrubbed.
func TestServeScrubsValueAfterReload(t *testing.T) {
	t.Parallel()
	cfg, state := dirs(t)
	up := storingUpstream(t)
	port := freePort(t)
	route := []string{routeYAML("api", port, up.URL)}
	writeNS(t, cfg, "t1", nsYAML([]string{secretYAML("api-key", "printf", "%s", apiKey)}, route))
	start(t, cfg, state)
	waitOK(t, state, "t1", "api-key")
	body := storeThenShow(t, port, func() {
		writeNS(t, cfg, "t1", nsYAML([]string{secretYAML("api-key", "sleep", "30")}, route))
		waitFor(t, 3*time.Second, "the reload", func() bool {
			return secretState(t, state, "t1", "api-key") == secrets.StatePending
		})
	})
	checkShown(t, body)
}

// A secret removed from the namespace file: its last value is still
// scrubbed, to the placeholder it had.
func TestServeScrubsValueOfRemovedSecret(t *testing.T) {
	t.Parallel()
	cfg, state := dirs(t)
	up := storingUpstream(t)
	port := freePort(t)
	route := []string{routeYAML("api", port, up.URL)}
	writeNS(t, cfg, "t1", nsYAML([]string{secretYAML("api-key", "printf", "%s", apiKey)}, route))
	start(t, cfg, state)
	waitOK(t, state, "t1", "api-key")
	body := storeThenShow(t, port, func() {
		writeNS(t, cfg, "t1", nsYAML([]string{secretYAML("other", "printf", "%s", otherKey)}, route))
		waitFor(t, 3*time.Second, "the reload", func() bool {
			n, ok := status(t, state, "t1")
			return ok && len(n.Secrets) == 1 && n.Secrets[0].Name == "other"
		})
	})
	checkShown(t, body)
}

// retiredOf returns the redactor's own retired slices of ns (not copies), so
// a test can see them wiped.
func (r *redactor) retiredOf(ns string) map[string][][]byte {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := map[string][][]byte{}
	if e := r.ns[ns]; e != nil {
		for name, vs := range e.retired {
			out[name] = slices.Clone(vs)
		}
	}
	return out
}

// The values a namespace retired are wiped when its file is removed.
func TestServeWipesRetiredValuesOnRemove(t *testing.T) {
	t.Parallel()
	cfg, state := dirs(t)
	b := testBroker(t, cfg, state)
	writeNS(t, cfg, "t1", nsYAML([]string{secretYAML("api-key", "printf", "%s", apiKey)}, nil))
	if err := b.scan(scanReload); err != nil {
		t.Fatal(err)
	}
	<-b.nss["t1"].fetched
	// A reload retires the value.
	writeNS(t, cfg, "t1", nsYAML([]string{secretYAML("api-key", "printf", "%s", otherKey)}, nil))
	if err := b.scan(scanReload); err != nil {
		t.Fatal(err)
	}
	<-b.nss["t1"].fetched
	retired := b.red.retiredOf("t1")
	if len(retired["api-key"]) != 1 || string(retired["api-key"][0]) != apiKey {
		t.Fatalf("retired: %d values of api-key", len(retired["api-key"]))
	}
	if err := os.Remove(filepath.Join(cfg, "t1.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := b.scan(scanReload); err != nil {
		t.Fatal(err)
	}
	for _, v := range retired["api-key"] {
		if !bytes.Equal(v, make([]byte, len(v))) {
			t.Error("a retired value was not wiped")
		}
	}
	if n := len(b.red.retiredOf("t1")); n != 0 {
		t.Errorf("t1 still has %d retired names", n)
	}
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

// The reload op retries a listener that could not start.
func TestServeReloadRetriesFailedListener(t *testing.T) {
	t.Parallel()
	cfg, state := dirs(t)
	up := newUpstream(t)
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	busy := taken.Addr().(*net.TCPAddr).Port
	port := freePort(t)
	writeNS(t, cfg, "t1", nsYAML(
		[]string{secretYAML("api-key", "printf", "%s", apiKey)},
		[]string{routeYAML("api", port, up.URL), routeYAML("idp", busy, up.URL)}))
	start(t, cfg, state)
	if n, _ := status(t, state, "t1"); !strings.Contains(n.Error, "idp") {
		t.Fatalf("error %q", n.Error)
	}
	_ = taken.Close()
	if _, err := do(t, state, admin.Request{Op: "reload"}); err != nil {
		t.Fatal(err)
	}
	n, _ := status(t, state, "t1")
	if len(n.Listeners) != 2 || n.Error != "" {
		t.Fatalf("status %+v", n)
	}
	waitOK(t, state, "t1", "api-key")
	if code, _, err := get(busy, "/v1", "sbxb_t1_api_key"); err != nil || code != http.StatusOK {
		t.Fatalf("idp route: %d %v", code, err)
	}
}

// The reload op retries a file refused for a port clash once the port is
// free.
func TestServeReloadRetriesPortClash(t *testing.T) {
	t.Parallel()
	cfg, state := dirs(t)
	up := newUpstream(t)
	port := freePort(t)
	writeNS(t, cfg, "t1", nsYAML(nil, []string{routeYAML("api", port, up.URL)}))
	start(t, cfg, state)
	waitFor(t, 3*time.Second, "t1", func() bool { _, ok := status(t, state, "t1"); return ok })
	writeNS(t, cfg, "t2", nsYAML(nil, []string{routeYAML("api", port, up.URL)}))
	waitFor(t, 3*time.Second, "the clash error", func() bool {
		return hasEvent(readLog(t, state), func(e eventlog.Event) bool {
			return e.Kind == eventlog.KindError && e.Namespace == "t2" && strings.Contains(e.Message, "is used by namespace t1")
		})
	})
	if err := os.Remove(filepath.Join(cfg, "t1.yaml")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "t1 to stop", func() bool { _, ok := status(t, state, "t1"); return !ok })
	if _, err := do(t, state, admin.Request{Op: "reload"}); err != nil {
		t.Fatal(err)
	}
	n, ok := status(t, state, "t2")
	if !ok || len(n.Listeners) != 1 {
		t.Fatalf("status %+v %v", n, ok)
	}
}

// runHeld runs Run with a hook that blocks after shutdown, before the log
// closes, until release is called. The hook gets the log.
func runHeld(t *testing.T, cfg, state string, hook func(*eventlog.Log)) (cancel func(), release func(), done <-chan error) {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	gate := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	errs := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		errs <- Run(ctx, Options{ConfigDir: cfg, StateDir: state, FetchTimeout: 10 * time.Second, PollInterval: 100 * time.Millisecond,
			beforeLogClose: func(l *eventlog.Log) {
				if hook != nil {
					hook(l)
				}
				<-gate
			}})
	}()
	t.Cleanup(func() {
		stop()
		release()
		select {
		case <-finished:
		case <-time.After(15 * time.Second):
			t.Error("Run did not end")
		}
	})
	waitFor(t, 3*time.Second, "the admin socket", func() bool { _, err := do(t, state, admin.Request{Op: "status"}); return err == nil })
	return stop, release, errs
}

// A late event (a listener may still write one after shutdown) is redacted
// with the values of the namespaces that ran until the end.
func TestShutdownKeepsRedactingUntilLogClose(t *testing.T) {
	t.Parallel()
	cfg, state := dirs(t)
	writeNS(t, cfg, "t1", nsYAML([]string{secretYAML("api-key", "printf", "%s", apiKey)}, nil))
	added := make(chan struct{})
	cancel, release, done := runHeld(t, cfg, state, func(l *eventlog.Log) {
		l.Add(eventlog.Event{Kind: eventlog.KindInfo, Namespace: "t1", Path: "/x/" + apiKey, Message: "late " + apiKey})
		close(added)
	})
	waitOK(t, state, "t1", "api-key")
	cancel()
	<-added
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !hasEvent(readLog(t, state), func(e eventlog.Event) bool { return strings.HasPrefix(e.Message, "late ") }) {
		t.Fatal("no late event")
	}
	noValues(t, state, apiKey)
}

// The lock is held for the broker's whole life: a second Run refuses while
// the first still shuts down, and starts once the first returned.
func TestLockHeldUntilRunReturns(t *testing.T) {
	t.Parallel()
	cfg, state := dirs(t)
	reached := make(chan struct{})
	cancel, release, done := runHeld(t, cfg, state, func(*eventlog.Log) { close(reached) })
	cancel()
	<-reached
	second := func() error {
		ctx, stop := context.WithCancel(context.Background())
		stop() // a Run that starts, then stops at once
		return Run(ctx, Options{ConfigDir: cfg, StateDir: state, PollInterval: 100 * time.Millisecond})
	}
	if err := second(); err == nil || err.Error() != "a broker already runs" {
		t.Fatalf("during shutdown: got %v", err)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// flock belongs to the open file description. A fetch command that
	// another test's broker in this process forks holds a copy of the lock
	// fd until its exec (close-on-exec acts only then), so the lock may stay
	// taken for a moment. A real broker has no fork in flight once Run
	// returned: cmd.Start returns after the exec, and Run waits for every
	// fetch.
	waitFor(t, 2*time.Second, "a second Run after the first returned", func() bool {
		err := second()
		if err != nil && err.Error() != "a broker already runs" {
			t.Fatal(err)
		}
		return err == nil
	})
}

// testBroker is a broker without Run, for tests that drive scan directly.
func testBroker(t *testing.T, cfg, state string) *broker {
	t.Helper()
	log, err := eventlog.Open(filepath.Join(state, logName), logRing, logMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	b := newBroker(Options{ConfigDir: cfg, StateDir: state, FetchTimeout: 10 * time.Second, PollInterval: time.Hour}, log)
	t.Cleanup(func() {
		b.fetchStop()
		for _, st := range b.nss {
			b.stop(st)
		}
		_ = log.Close()
	})
	return b
}

// On a reload the new listener binds a port the old one just closed; a
// fetch command forked a moment before may hold the old socket until its
// exec. So a port the old state held is retried for a short while, and any
// other busy port fails at once.
func TestListenRetriesHeldPort(t *testing.T) {
	t.Parallel()
	cfg, state := dirs(t)
	b := testBroker(t, cfg, state)
	store := nsSecrets{Store: secrets.NewStore("t1", nil, time.Second, b.log, nil), ns: "t1", red: b.red}
	hold := func() (int, net.Listener) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		return ln.Addr().(*net.TCPAddr).Port, ln
	}
	port, ln := hold()
	up := &url.URL{Scheme: "http", Host: "127.0.0.1:1"}
	if _, err := b.listen("t1", config.Route{Name: "api", Listen: port, Upstream: up}, store, false); err == nil {
		t.Fatal("a busy port that the old state did not hold was retried")
	}
	_ = ln.Close()
	port, ln = hold()
	time.AfterFunc(200*time.Millisecond, func() { _ = ln.Close() })
	h, err := b.listen("t1", config.Route{Name: "api", Listen: port, Upstream: up}, store, true)
	if err != nil {
		t.Fatal(err)
	}
	_ = h.Close()
}

// An editor that truncates the file, then writes it: a poll that sees the
// empty file must not act on it. A change counts once two polls in a row
// saw the same stamp.
func TestScanWaitsForStableFile(t *testing.T) {
	t.Parallel()
	cfg, state := dirs(t)
	up := newUpstream(t)
	content := nsYAML([]string{secretYAML("api-key", "printf", "%s", apiKey)},
		[]string{routeYAML("api", freePort(t), up.URL)})
	writeNS(t, cfg, "t1", content)
	b := testBroker(t, cfg, state)
	if err := b.scan(scanFirst); err != nil {
		t.Fatal(err)
	}
	st0 := b.nss["t1"]
	<-st0.fetched
	path := filepath.Join(cfg, "t1.yaml")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = b.scan(scanPoll)
	if b.nss["t1"] != st0 || st0.ctx.Err() != nil {
		t.Fatal("a poll acted on the empty file")
	}
	if err := os.WriteFile(path, []byte(content+"# edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = b.scan(scanPoll)
	if b.nss["t1"] != st0 {
		t.Fatal("a poll acted on a stamp seen once")
	}
	_ = b.scan(scanPoll)
	st1 := b.nss["t1"]
	if st1 == st0 || len(st1.cfg.Secrets) != 1 {
		t.Fatal("the stable file was not loaded")
	}
	var reloads []string
	for _, e := range b.log.Recent(1000) {
		if e.Kind == eventlog.KindReload {
			reloads = append(reloads, e.Message)
		}
	}
	if !slices.Equal(reloads, []string{"loaded", "reloaded"}) {
		t.Fatalf("reload events %v", reloads)
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
