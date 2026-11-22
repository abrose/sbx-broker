package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abrose/sbx-broker/internal/config"
	"github.com/abrose/sbx-broker/internal/eventlog"
	"github.com/abrose/sbx-broker/internal/secrets"
	"github.com/abrose/sbx-broker/internal/transform"
)

const (
	apiKey = "real-api-key-1"
	pass   = "p@ss word/1+&"
	ns     = "t1"
)

// testValues are the ok values of the fake namespace t1, by secret name.
var testValues = map[string]string{"api-key": apiKey, "pass": pass}

type fakeSecret struct {
	value string
	state secrets.State
}

type fakeSecrets map[string]fakeSecret

func (f fakeSecrets) Lookup(name string) ([]byte, secrets.State) {
	s, ok := f[name]
	if !ok {
		return nil, secrets.StateUndefined
	}
	if s.state != secrets.StateOK {
		return nil, s.state
	}
	return []byte(s.value), secrets.StateOK
}

func (f fakeSecrets) Values() map[string][]byte {
	out := map[string][]byte{}
	for name, s := range f {
		if s.state == secrets.StateOK {
			out[name] = []byte(s.value)
		}
	}
	return out
}

func t1() fakeSecrets {
	f := fakeSecrets{"slow": {state: secrets.StatePending}}
	for name, v := range testValues {
		f[name] = fakeSecret{value: v, state: secrets.StateOK}
	}
	return f
}

// leaks returns the names of the test values found in s, in plain form or
// in the escapes the broker sends.
func leaks(s string) []string {
	var found []string
	for _, name := range slices.Sorted(maps.Keys(testValues)) {
		v := testValues[name]
		if strings.Contains(s, v) || strings.Contains(s, url.QueryEscape(v)) || strings.Contains(s, url.PathEscape(v)) {
			found = append(found, name)
		}
	}
	return found
}

// noLeak fails the test when s holds a test value. It prints names only.
func noLeak(t *testing.T, what, s string) {
	t.Helper()
	if found := leaks(s); len(found) > 0 {
		t.Errorf("%s holds the value of %v", what, found)
	}
}

func swapTransform(t *testing.T) transform.Transform {
	t.Helper()
	tr, err := transform.New(config.TransformSpec{Name: "swap"})
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// freePort returns a port that was free a moment ago.
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

func openLog(t *testing.T) *eventlog.Log {
	t.Helper()
	l, err := eventlog.Open(filepath.Join(t.TempDir(), "broker.log"), 1000, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// newProxy makes the listener of route api of t1 with the swap transform
// unless trs is given. It does not start it.
func newProxy(t *testing.T, upstream string, trs ...transform.Transform) (*HTTP, *eventlog.Log) {
	t.Helper()
	u, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	if len(trs) == 0 {
		trs = []transform.Transform{swapTransform(t)}
	}
	l := openLog(t)
	route := config.Route{Name: "api", Listen: freePort(t), Upstream: u, Transforms: []config.TransformSpec{{Name: "swap"}}}
	return NewHTTP(ns, route, t1(), trs, l), l
}

// startProxy makes and starts the listener; the test's cleanup closes it.
func startProxy(t *testing.T, upstream string, trs ...transform.Transform) (*HTTP, *eventlog.Log) {
	t.Helper()
	h, l := newProxy(t, upstream, trs...)
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h, l
}

func proxyURL(h *HTTP, path string) string { return "http://" + h.Addr() + path }

// do sends req with a client that adds no Accept-Encoding of its own and
// returns the status, the headers and the body.
func do(t *testing.T, req *http.Request) (int, http.Header, string) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, string(body)
}

func get(t *testing.T, target string, header map[string]string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	return do(t, req)
}

// waitEvent waits up to 3 s for an event of kind and returns the first one.
func waitEvent(t *testing.T, l *eventlog.Log, kind eventlog.Kind) eventlog.Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		for _, e := range l.Recent(1000) {
			if e.Kind == kind {
				return e
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s event", kind)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func eventsOf(l *eventlog.Log, kind eventlog.Kind) []eventlog.Event {
	var out []eventlog.Event
	for _, e := range l.Recent(1000) {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// noLeakInLog checks every event of the log for a value.
func noLeakInLog(t *testing.T, l *eventlog.Log) {
	t.Helper()
	data, err := json.Marshal(l.Recent(1000))
	if err != nil {
		t.Fatal(err)
	}
	noLeak(t, "the event log", string(data))
}

func TestProxySwapsAndScrubs(t *testing.T) {
	var seen atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.Header.Get("Authorization"))
		w.Header().Set("X-Echo", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "you sent "+r.Header.Get("Authorization"))
	}))
	defer up.Close()
	h, l := startProxy(t, up.URL)

	status, header, body := get(t, proxyURL(h, "/v1/items"), map[string]string{"Authorization": "Bearer sbxb_t1_api_key"})

	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	if got, _ := seen.Load().(string); got != "Bearer "+apiKey {
		t.Errorf("the upstream did not get the swapped value (leaks %v)", leaks(got))
	}
	if got := header.Get("X-Echo"); got != "Bearer sbxb_t1_api_key" {
		noLeak(t, "the echo header", got)
		t.Errorf("the echo header is not the placeholder")
	}
	if body != "you sent Bearer sbxb_t1_api_key" {
		noLeak(t, "the body", body)
		t.Errorf("the body does not hold the placeholder")
	}
	waitEvent(t, l, eventlog.KindSwap)
	noLeakInLog(t, l)
}

func TestProxyHost(t *testing.T) {
	type seenReq struct{ host, xff, fwd string }
	got := make(chan seenReq, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- seenReq{r.Host, r.Header.Get("X-Forwarded-For"), r.Header.Get("X-Forwarded-Host")}
	}))
	defer up.Close()
	h, _ := startProxy(t, up.URL)

	get(t, proxyURL(h, "/"), map[string]string{"X-Forwarded-For": "10.1.1.1"})

	s := <-got
	if want := strings.TrimPrefix(up.URL, "http://"); s.host != want {
		t.Errorf("upstream saw Host %q, want %q", s.host, want)
	}
	if s.xff != "" || s.fwd != "" {
		t.Errorf("upstream saw X-Forwarded-For %q, X-Forwarded-Host %q", s.xff, s.fwd)
	}
}

func TestProxyRefusals(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer up.Close()
	h, l := startProxy(t, up.URL)

	cases := []struct {
		name, auth string
		status     int
		body       string
	}{
		{"other namespace", "Bearer sbxb_t2_api_key", 403, `{"error":"placeholder of another namespace","namespace":"t2"}`},
		{"not loaded", "Bearer sbxb_t1_slow", 503, `{"error":"secret not loaded","secret":"slow","state":"pending"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, header, body := get(t, proxyURL(h, "/x"), map[string]string{"Authorization": c.auth})
			if status != c.status || body != c.body {
				t.Errorf("got %d %s, want %d %s", status, body, c.status, c.body)
			}
			if ct := header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type %q", ct)
			}
		})
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("the upstream counted %d requests", n)
	}
	refuse := eventsOf(l, eventlog.KindRefuse)
	if len(refuse) != 2 {
		t.Fatalf("%d refuse events, want 2", len(refuse))
	}
	if refuse[0].Status != 403 || refuse[1].Status != 503 || refuse[0].Route != "api" || refuse[0].Path != "/x" {
		t.Errorf("refuse events %+v", refuse)
	}
	if !slices.Equal(refuse[1].Secrets, []string{"slow"}) {
		t.Errorf("503 refuse event names %v", refuse[1].Secrets)
	}
	if v := eventsOf(l, eventlog.KindViolation); len(v) != 1 || v[0].Status != 403 {
		t.Errorf("violation events %+v, want one for the 403", v)
	}
	if n := len(eventsOf(l, eventlog.KindSwap)); n != 0 {
		t.Errorf("%d swap events for refused requests", n)
	}
}

func TestProxyDropsAcceptEncoding(t *testing.T) {
	got := make(chan []string, 2)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Values("Accept-Encoding")
	}))
	defer up.Close()
	h, _ := startProxy(t, up.URL)

	get(t, proxyURL(h, "/"), map[string]string{"Accept-Encoding": "gzip"})
	if ae := <-got; len(ae) != 0 {
		t.Errorf("upstream saw Accept-Encoding %v after the app sent gzip", ae)
	}
	// With none from the app, the broker's own transport must add none.
	get(t, proxyURL(h, "/"), nil)
	if ae := <-got; len(ae) != 0 {
		t.Errorf("upstream saw Accept-Encoding %v after the app sent none", ae)
	}
}

func TestProxyStreamsWithoutBuffering(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: a\n\n")
		http.NewResponseController(w).Flush()
		time.Sleep(500 * time.Millisecond)
		_, _ = io.WriteString(w, "data: b\n\n")
	}))
	defer up.Close()
	h, _ := startProxy(t, up.URL)

	start := time.Now()
	resp, err := http.Get(proxyURL(h, "/events"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	first, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d >= 300*time.Millisecond {
		t.Errorf("first event after %v", d)
	}
	if first != "data: a\n" {
		t.Errorf("first line %q", first)
	}
	rest, _ := io.ReadAll(br)
	if string(rest) != "\ndata: b\n\n" {
		t.Errorf("rest %q", rest)
	}
}

func TestProxyUpstreamDown(t *testing.T) {
	h, l := startProxy(t, "http://127.0.0.1:"+strconv.Itoa(freePort(t)))

	status, header, body := get(t, proxyURL(h, "/v1/sbxb_t1_api_key?k=sbxb_t1_pass"), map[string]string{"Authorization": "Bearer sbxb_t1_api_key"})

	if want := `{"error":"upstream unreachable","route":"api"}`; status != 502 || body != want {
		t.Errorf("got %d %s, want 502 %s", status, body, want)
	}
	if ct := header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type %q", ct)
	}
	e := waitEvent(t, l, eventlog.KindUpstream)
	if e.Route != "api" || e.Message == "" {
		t.Errorf("upstream event %+v", e)
	}
	waitEvent(t, l, eventlog.KindSwap)
	noLeakInLog(t, l)
}

func TestProxyLoopbackOnly(t *testing.T) {
	h, _ := startProxy(t, "http://127.0.0.1:1")
	if !strings.HasPrefix(h.Addr(), "127.0.0.1:") {
		t.Errorf("Addr %q", h.Addr())
	}
}

// recorder runs another transform and keeps the Used slices it returns.
type recorder struct {
	transform.Transform
	mu   sync.Mutex
	used [][]byte
}

func (r *recorder) Request(req *http.Request, ns string, sec transform.Secrets) (transform.Result, error) {
	res, err := r.Transform.Request(req, ns, sec)
	r.mu.Lock()
	for _, v := range res.Used {
		r.used = append(r.used, v)
	}
	r.mu.Unlock()
	return res, err
}

// wiped checks that the recorder saw Used values and that every byte of
// them is zero now.
func (r *recorder) wiped(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.used) == 0 {
		t.Fatal("the transform returned no Used values")
	}
	for i, v := range r.used {
		if len(v) == 0 || bytes.Count(v, []byte{0}) != len(v) {
			t.Errorf("Used slice %d is not wiped", i)
		}
	}
}

// refuser refuses every request with a 503.
type refuser struct{}

func (refuser) Name() string { return "refuser" }
func (refuser) Request(*http.Request, string, transform.Secrets) (transform.Result, error) {
	return transform.Result{}, &transform.Refusal{Status: 503, Body: map[string]string{"error": "secret not loaded", "secret": "x", "state": "pending"}}
}
func (refuser) Response(*http.Response, string, transform.Secrets, map[string][]byte, func([]string)) error {
	return nil
}

func TestProxyWipesUsedValues(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Header.Get("Authorization"))
	}))
	defer up.Close()
	auth := map[string]string{"Authorization": "Bearer sbxb_t1_api_key", "X-Pass": "sbxb_t1_pass"}

	t.Run("after the answer", func(t *testing.T) {
		rec := &recorder{Transform: swapTransform(t)}
		h, l := startProxy(t, up.URL, rec)
		get(t, proxyURL(h, "/"), auth)
		waitEvent(t, l, eventlog.KindSwap)
		rec.wiped(t)
	})
	t.Run("after a refusal", func(t *testing.T) {
		rec := &recorder{Transform: swapTransform(t)}
		h, l := startProxy(t, up.URL, rec, refuser{})
		if status, _, _ := get(t, proxyURL(h, "/"), auth); status != 503 {
			t.Fatalf("status %d", status)
		}
		waitEvent(t, l, eventlog.KindRefuse)
		rec.wiped(t)
	})
	t.Run("after an upstream failure", func(t *testing.T) {
		rec := &recorder{Transform: swapTransform(t)}
		h, l := startProxy(t, "http://127.0.0.1:"+strconv.Itoa(freePort(t)), rec)
		get(t, proxyURL(h, "/"), auth)
		waitEvent(t, l, eventlog.KindSwap)
		rec.wiped(t)
	})
}

// failer fails every request with an error (not a refusal) that quotes the
// value of api-key.
type failer struct{}

func (failer) Name() string { return "failer" }
func (failer) Request(*http.Request, string, transform.Secrets) (transform.Result, error) {
	return transform.Result{}, errors.New("cannot read " + apiKey)
}
func (failer) Response(*http.Response, string, transform.Secrets, map[string][]byte, func([]string)) error {
	return nil
}

// dropped is the store after a refresh dropped every value: Lookup still
// answered for the request, but Values has nothing for the scrub.
type dropped struct{ fakeSecrets }

func (dropped) Values() map[string][]byte { return map[string][]byte{} }

func TestProxyTransformErrorKeepsValuesOut(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	l := openLog(t)
	rec := &recorder{Transform: swapTransform(t)}
	h := NewHTTP(ns, config.Route{Name: "api", Listen: freePort(t), Upstream: u}, dropped{t1()}, []transform.Transform{rec, failer{}}, l)
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })

	status, _, body := get(t, proxyURL(h, "/"), map[string]string{"Authorization": "Bearer sbxb_t1_api_key"})

	if want := `{"error":"bad request","route":"api"}`; status != 400 || body != want {
		noLeak(t, "the answer", body)
		t.Errorf("got %d, want 400 %s", status, want)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("the upstream counted %d requests", n)
	}
	waitEvent(t, l, eventlog.KindError)
	noLeakInLog(t, l)
	rec.wiped(t)
}

func TestProxyInformational(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := r.Header.Get("Authorization")
		w.Header().Set("Link", "<https://cdn.example/a?k="+v+">; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Del("Link")
		w.Header().Set("X-Echo", v)
		w.WriteHeader(http.StatusContinue)
		w.Header().Del("X-Echo")
		_, _ = io.WriteString(w, "ok")
	}))
	defer up.Close()
	h, _ := startProxy(t, up.URL)

	type info struct {
		code   int
		header http.Header
	}
	var mu sync.Mutex
	var infos []info
	req, err := http.NewRequest(http.MethodGet, proxyURL(h, "/"), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "sbxb_t1_api_key")
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		Got1xxResponse: func(code int, h textproto.MIMEHeader) error {
			mu.Lock()
			defer mu.Unlock()
			infos = append(infos, info{code, http.Header(h).Clone()})
			return nil
		},
	}))
	status, _, body := do(t, req)
	if status != 200 || body != "ok" {
		t.Fatalf("got %d %q", status, body)
	}
	mu.Lock()
	defer mu.Unlock()
	saw100 := false
	for _, in := range infos {
		var b strings.Builder
		_ = in.header.Write(&b)
		noLeak(t, "a "+strconv.Itoa(in.code)+" header", b.String())
		switch {
		case in.code == http.StatusContinue:
			saw100 = true
			if len(in.header) != 0 {
				t.Errorf("100 Continue reached the app with headers %v", slices.Sorted(maps.Keys(in.header)))
			}
		default:
			t.Errorf("a %d answer reached the app", in.code)
		}
	}
	if !saw100 {
		t.Errorf("100 Continue did not reach the app")
	}
}

func TestProxyUnsupportedEncoding(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "br")
		_, _ = io.WriteString(w, "compressed bytes")
	}))
	defer up.Close()
	h, l := startProxy(t, up.URL)

	status, _, body := get(t, proxyURL(h, "/"), nil)

	if want := `{"error":"unsupported content encoding","route":"api"}`; status != 502 || body != want {
		t.Errorf("got %d %s, want 502 %s", status, body, want)
	}
	e := waitEvent(t, l, eventlog.KindUpstream)
	if e.Message != "unsupported content encoding br" {
		t.Errorf("upstream event message %q", e.Message)
	}
}

func TestProxyEvents(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, r.Header.Get("Authorization")+" "+pass)
	}))
	defer up.Close()
	h, l := startProxy(t, up.URL)

	req, err := http.NewRequest(http.MethodPost, proxyURL(h, "/v1/sbxb_t1_api_key/x?k=sbxb_t1_api_key"), strings.NewReader("\x00\x01"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sbxb_t1_api_key")
	req.Header.Set("Content-Type", "application/octet-stream")
	if status, _, _ := do(t, req); status != http.StatusCreated {
		t.Fatalf("status %d", status)
	}

	e := waitEvent(t, l, eventlog.KindSwap)
	if e.Namespace != ns || e.Route != "api" || e.Method != "POST" || e.Status != 201 || e.DurationMS < 0 {
		t.Errorf("swap event %+v", e)
	}
	if e.Path != "/v1/sbxb_t1_api_key/x" {
		noLeak(t, "the event path", e.Path)
		t.Errorf("event path is not the incoming path without the query")
	}
	if want := []string{"api-key", "pass"}; !slices.Equal(e.Secrets, want) {
		t.Errorf("swap event names %v, want %v (swapped and scrubbed)", e.Secrets, want)
	}
	if n := len(eventsOf(l, eventlog.KindSwap)); n != 1 {
		t.Errorf("%d swap events, want 1", n)
	}
	u := waitEvent(t, l, eventlog.KindUnscanned)
	if u.Message != "application/octet-stream" || u.Route != "api" {
		t.Errorf("unscanned event %+v", u)
	}
	noLeakInLog(t, l)
}

func TestProxyRefusesProtocolSwitch(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, bw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = bw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: x-test\r\n\r\n")
		_ = bw.Flush()
		_, _ = io.Copy(io.Discard, conn)
	}))
	defer up.Close()
	h, l := startProxy(t, up.URL)

	status, _, body := get(t, proxyURL(h, "/"), map[string]string{"Connection": "Upgrade", "Upgrade": "x-test"})

	if want := `{"error":"protocol switch not supported","route":"api"}`; status != 502 || body != want {
		t.Errorf("got %d %s, want 502 %s", status, body, want)
	}
	waitEvent(t, l, eventlog.KindUpstream)
	waitEvent(t, l, eventlog.KindSwap)
}

func TestProxyCloseWaitsForOpenRequests(t *testing.T) {
	arrived := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, "ok")
	}))
	defer up.Close()
	h, _ := newProxy(t, up.URL)
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}

	type answer struct {
		status int
		body   string
	}
	got := make(chan answer, 1)
	go func() {
		resp, err := http.Get(proxyURL(h, "/"))
		if err != nil {
			got <- answer{}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		got <- answer{resp.StatusCode, string(b)}
	}()
	<-arrived
	if err := h.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if a := <-got; a.status != 200 || a.body != "ok" {
		t.Errorf("open request got %d %q", a.status, a.body)
	}
	if _, err := net.DialTimeout("tcp", h.Addr(), time.Second); err == nil {
		t.Errorf("the listener still accepts after Close")
	}
}

func TestProxyCloseCutsLongStreams(t *testing.T) {
	stop := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "data: a\n\n")
		http.NewResponseController(w).Flush()
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	defer up.Close()
	defer close(stop)
	h, _ := newProxy(t, up.URL)
	h.closeWait = 200 * time.Millisecond
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(proxyURL(h, "/"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_ = h.Close()
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Close took %v", d)
	}
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Errorf("the stream ended cleanly; want it cut")
	}
}

func TestProxyStartFailsOnTakenPort(t *testing.T) {
	h, _ := startProxy(t, "http://127.0.0.1:1")
	_, port, _ := net.SplitHostPort(h.Addr())
	u, _ := url.Parse("http://127.0.0.1:1")
	p, _ := strconv.Atoi(port)
	other := NewHTTP(ns, config.Route{Name: "api", Listen: p, Upstream: u}, t1(), []transform.Transform{swapTransform(t)}, openLog(t))
	if err := other.Start(); err == nil {
		_ = other.Close()
		t.Errorf("Start on a taken port did not fail")
	}
}
