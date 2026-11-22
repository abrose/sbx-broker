// Package proxy runs the broker's listeners. An HTTP listener takes the
// requests of one route, runs the route's transforms on each request and on
// its answer, and forwards it to the route's fixed upstream.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/abrose/sbx-broker/internal/cloexec"
	"github.com/abrose/sbx-broker/internal/config"
	"github.com/abrose/sbx-broker/internal/eventlog"
	"github.com/abrose/sbx-broker/internal/transform"
)

const (
	readHeaderTimeout = 10 * time.Second
	defaultCloseWait  = 5 * time.Second
	handlerWait       = 2 * time.Second // after the forced close
)

// HTTP is the listener of one HTTP route.
type HTTP struct {
	ns        string
	route     config.Route
	sec       transform.Secrets
	trs       []transform.Transform
	log       *eventlog.Log
	transport *http.Transport
	closeWait time.Duration

	mu     sync.Mutex
	srv    *http.Server
	addr   string
	active int           // handlers that run now
	idle   chan struct{} // closed when active drops to 0; set by Close
}

// NewHTTP returns the listener of route in namespace ns. trs run in order on
// each request and in reverse order on each answer.
func NewHTTP(ns string, route config.Route, sec transform.Secrets, trs []transform.Transform, log *eventlog.Log) *HTTP {
	t := http.DefaultTransport.(*http.Transport).Clone()
	// The transport would ask for gzip by itself; the broker wants plain
	// answers to scrub. The swap transform decodes an upstream that
	// compresses anyway.
	t.DisableCompression = true
	// Never a proxy from HTTP_PROXY or HTTPS_PROXY: the swapped request,
	// with its values, goes to the route's upstream only.
	t.Proxy = nil
	return &HTTP{ns: ns, route: route, sec: sec, trs: trs, log: log, transport: t, closeWait: defaultCloseWait}
}

// Start listens on 127.0.0.1:<route.Listen> and serves in the background.
func (h *HTTP) Start() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.srv != nil {
		return fmt.Errorf("route %s: already started", h.route.Name)
	}
	raw, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(h.route.Listen)))
	if err != nil {
		return fmt.Errorf("route %s: %w", h.route.Name, err)
	}
	// A fetch command must not inherit an app's connection: it would keep
	// the connection (and the port) open until the command ends.
	ln, err := cloexec.Listener(raw)
	if err != nil {
		_ = raw.Close()
		return fmt.Errorf("route %s: %w", h.route.Name, err)
	}
	errLog := log.New(eventWriter{h}, "", 0)
	h.srv = &http.Server{Handler: h, ReadHeaderTimeout: readHeaderTimeout, ErrorLog: errLog}
	h.addr = ln.Addr().String()
	go func() { _ = h.srv.Serve(ln) }()
	return nil
}

// Addr returns the address the listener listens on, once started.
func (h *HTTP) Addr() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.addr
}

// Close stops accepting, waits for open requests at most 5 s, then closes
// the connections that are still open and waits up to 2 s more for their
// handlers to end. After Close returns, the listener writes no more events,
// unless those 2 s passed: then a late event (swap, upstream) may still come.
func (h *HTTP) Close() error {
	h.mu.Lock()
	srv := h.srv
	h.mu.Unlock()
	if srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.closeWait)
	defer cancel()
	err := srv.Shutdown(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		err = srv.Close()
	}
	h.waitHandlers(handlerWait)
	h.transport.CloseIdleConnections()
	return err
}

// waitHandlers waits until no handler runs, at most d. A handler cut by
// srv.Close still writes its events on the way out.
func (h *HTTP) waitHandlers(d time.Duration) {
	h.mu.Lock()
	if h.active == 0 {
		h.mu.Unlock()
		return
	}
	if h.idle == nil {
		h.idle = make(chan struct{})
	}
	idle := h.idle
	h.mu.Unlock()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-idle:
	case <-timer.C:
	}
}

func (h *HTTP) enter() {
	h.mu.Lock()
	h.active++
	h.mu.Unlock()
}

func (h *HTTP) leave() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.active--
	if h.active == 0 && h.idle != nil {
		close(h.idle)
		h.idle = nil
	}
}

// ServeHTTP runs the request transforms, then forwards the request.
func (h *HTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.enter()
	defer h.leave()
	// Taken before the transforms: they may put values into both.
	x := &exchange{h: h, start: time.Now(), method: r.Method, path: eventPath(r.URL), names: map[string]bool{}}
	contentType := r.Header.Get("Content-Type")
	for _, tr := range h.trs {
		res, err := tr.Request(r, h.ns, h.sec)
		x.addUsed(res.Used)
		if err != nil {
			h.refuse(w, x, tr, err)
			return
		}
		x.addNames(res.Swapped)
		if res.Unscanned {
			h.add(eventlog.Event{Kind: eventlog.KindUnscanned, Method: x.method, Path: x.path, Direction: "request", Message: contentType})
		}
	}
	h.proxy(x).ServeHTTP(noInformational{w}, r)
}

// eventPath is the path for an event: the broker's own escape of the
// decoded path (no control byte, no space), not the client's RawPath. The
// server's redactor knows a value in this form too.
func eventPath(u *url.URL) string {
	return (&url.URL{Path: u.Path}).EscapedPath()
}

// noInformational keeps the upstream's 1xx answers from the app (ruling
// R10). ReverseProxy forwards them from a client trace, and their headers
// never pass through ModifyResponse, so they are never scrubbed. A 100
// Continue goes on with no headers; every other 1xx (such as 103 Early
// Hints) is dropped. A 101 is a final answer and does not come here.
type noInformational struct{ http.ResponseWriter }

func (w noInformational) WriteHeader(code int) {
	if code >= 100 && code <= 199 && code != http.StatusSwitchingProtocols {
		// The proxy copied the 1xx headers into the header map; the
		// final answer must not carry them either.
		clear(w.Header())
		if code != http.StatusContinue {
			return
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach Flush of the real writer.
func (w noInformational) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// refuse answers a request that a transform did not let through, after it
// wipes the values the transforms before it returned.
func (h *HTTP) refuse(w http.ResponseWriter, x *exchange, tr transform.Transform, err error) {
	var ref *transform.Refusal
	if !errors.As(err, &ref) {
		// Scrubbed before the wipe: a value may be in used alone.
		msg := h.scrubText(fmt.Sprintf("transform %s: %v", tr.Name(), err), x.used)
		x.wipe()
		h.add(eventlog.Event{Kind: eventlog.KindError, Method: x.method, Path: x.path, Direction: "request", Message: msg})
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request", "route": h.route.Name})
		return
	}
	x.wipe() // a Refusal holds names only
	e := eventlog.Event{Kind: eventlog.KindRefuse, Method: x.method, Path: x.path, Direction: "request",
		Status: ref.Status, Message: ref.Body["error"]}
	if name := ref.Body["secret"]; name != "" {
		e.Secrets = []string{name}
	}
	h.add(e)
	if ref.Status == http.StatusForbidden {
		h.add(eventlog.Event{Kind: eventlog.KindViolation, Method: x.method, Path: x.path, Direction: "request",
			Status: ref.Status, Message: "placeholder of namespace " + ref.Body["namespace"]})
	}
	writeJSON(w, ref.Status, ref.Body)
}

// proxy returns the reverse proxy for one exchange.
func (h *HTTP) proxy(x *exchange) *httputil.ReverseProxy {
	up := h.route.Upstream
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// Rewrite builds Out from In.URL (the swapped URL), never
			// from RequestURI. X-Forwarded-* are gone already.
			pr.SetURL(up)
			pr.Out.Host = up.Host
			pr.Out.Header.Del("Accept-Encoding")
		},
		Transport:     h.transport,
		FlushInterval: -1,
		ErrorLog:      log.New(eventWriter{h}, "", 0),
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode == http.StatusSwitchingProtocols {
				// The raw stream after a 101 would bypass every
				// transform. The proxy closes the body.
				return errProtocolSwitch
			}
			for i := len(h.trs) - 1; i >= 0; i-- {
				if err := h.trs[i].Response(resp, h.ns, h.sec, x.used, x.addNames); err != nil {
					return responseError{err}
				}
			}
			x.status = resp.StatusCode
			resp.Body = &finalBody{ReadCloser: resp.Body, x: x}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			body := "upstream unreachable"
			var re responseError
			switch {
			case errors.Is(err, errProtocolSwitch):
				body = errProtocolSwitch.Error()
			case errors.As(err, &re):
				body = "unsupported content encoding"
			}
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": body, "route": h.route.Name})
			h.add(eventlog.Event{Kind: eventlog.KindUpstream, Method: x.method, Path: x.path,
				Status: http.StatusBadGateway, Message: h.scrubText(err.Error(), x.used)})
			x.finish(http.StatusBadGateway)
		},
	}
}

var errProtocolSwitch = errors.New("protocol switch not supported")

// responseError is an error of a response transform: the answer cannot be
// scrubbed (an unsupported Content-Encoding). Its text is scrubbed already.
type responseError struct{ err error }

func (e responseError) Error() string { return e.err.Error() }
func (e responseError) Unwrap() error { return e.err }

// exchange is the state of one forwarded request.
type exchange struct {
	h            *HTTP
	start        time.Time
	method, path string
	used         map[string][]byte // the union of the transforms' Used
	copies       [][]byte          // every Used slice, for the wipe
	status       int
	mu           sync.Mutex
	names        map[string]bool // swapped and scrubbed
	once         sync.Once
}

func (x *exchange) addUsed(used map[string][]byte) {
	for name, v := range used {
		if x.used == nil {
			x.used = map[string][]byte{}
		}
		if _, ok := x.used[name]; !ok {
			x.used[name] = v
		}
		x.copies = append(x.copies, v)
	}
}

func (x *exchange) addNames(names []string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, n := range names {
		x.names[n] = true
	}
}

// wipe zeroes every copy of a value the transforms returned (ruling R8).
func (x *exchange) wipe() {
	for _, v := range x.copies {
		clear(v)
	}
}

// finish wipes the values and writes the swap event, once.
func (x *exchange) finish(status int) {
	x.once.Do(func() {
		x.wipe()
		x.mu.Lock()
		names := make([]string, 0, len(x.names))
		for n := range x.names {
			names = append(names, n)
		}
		x.mu.Unlock()
		slices.Sort(names)
		x.h.add(eventlog.Event{Kind: eventlog.KindSwap, Method: x.method, Path: x.path, Secrets: names,
			Status: status, DurationMS: time.Since(x.start).Milliseconds()})
	})
}

// finalBody is the answer's body after every transform. Its Close ends the
// exchange: the transforms' done funcs have run by then.
type finalBody struct {
	io.ReadCloser
	x *exchange
}

func (b *finalBody) Close() error {
	err := b.ReadCloser.Close()
	b.x.finish(b.x.status)
	return err
}

// add records e with the namespace and route of h.
func (h *HTTP) add(e eventlog.Event) {
	if h.log == nil {
		return
	}
	e.Namespace, e.Route = h.ns, h.route.Name
	h.log.Add(e)
}

// scrubText replaces each value of used and of the namespace in s, raw or
// in the escapes of a URL, with its name, for error texts that go to the
// log. Longer values first, so a value inside another goes with it.
func (h *HTTP) scrubText(s string, used map[string][]byte) string {
	type form struct{ text, name string }
	var forms []form
	for _, vals := range []map[string][]byte{used, h.sec.Values()} {
		for name, v := range vals {
			if len(v) == 0 {
				continue
			}
			for _, f := range []string{string(v), url.PathEscape(string(v)), url.QueryEscape(string(v))} {
				forms = append(forms, form{f, "<" + name + ">"})
			}
		}
	}
	slices.SortFunc(forms, func(a, b form) int { return len(b.text) - len(a.text) })
	for _, f := range forms {
		s = strings.ReplaceAll(s, f.text, f.name)
	}
	return s
}

// eventWriter turns the lines of the server's and the proxy's error logs
// into error events, so nothing goes to stderr.
type eventWriter struct{ h *HTTP }

func (e eventWriter) Write(p []byte) (int, error) {
	e.h.add(eventlog.Event{Kind: eventlog.KindError, Message: e.h.scrubText(strings.TrimSpace(string(p)), nil)})
	return len(p), nil
}

func writeJSON(w http.ResponseWriter, status int, body map[string]string) {
	data, _ := json.Marshal(body) // a map of strings always encodes; keys sorted
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
