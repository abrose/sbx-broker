//go:build integration

// Package integration proves the broker end to end: the broker of
// internal/server runs on this host, the recorder mock runs in a container,
// and the cases of test/cases go through the broker's routes to it.
package integration

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/abrose/sbx-broker/internal/eventlog"
	"github.com/abrose/sbx-broker/test/cases"
	"github.com/abrose/sbx-broker/test/harness"
)

// check tells what is wrong with the record of a case, or nil.
type check func(harness.Record) error

// expect is what one case must give: its answer's status, its one event in
// broker.log, and, for a swap case, what its record must hold.
type expect struct {
	status  int
	kind    eventlog.Kind
	route   string
	method  string
	path    string
	secrets []string // the names in the event
	record  check    // nil: the request must not reach the recorder
}

const (
	api = harness.ValueAPIKey
	cs  = harness.ValueClientSecret
	old = harness.ValueOldSecret
	pw  = harness.ValuePass
)

var expects = map[string]expect{
	"header":        {200, eventlog.KindSwap, "api", "GET", "/echo", []string{"api-key"}, headerIs("Authorization", "Bearer "+api)},
	"base64-header": {200, eventlog.KindSwap, "api", "GET", "/echo", []string{"api-key"}, base64HeaderIs("X-Api-Key", api)},
	"basic":         {200, eventlog.KindSwap, "api", "GET", "/echo", []string{"pass"}, basicIs("user", pw)},
	"query":         {200, eventlog.KindSwap, "api", "GET", "/echo", []string{"pass"}, queryIs("key", pw)},
	"path":          {200, eventlog.KindSwap, "api", "GET", "/echo/sbxb_t1_pass", []string{"pass"}, pathIs("/echo/" + pw)},
	"form-token":    {200, eventlog.KindSwap, "idp", "POST", cases.TokenPath, []string{"client-secret"}, formIs("client_secret", cs)},
	"form-token-rejected": {401, eventlog.KindSwap, "idp", "POST", cases.TokenPath, []string{"old-secret"},
		formIs("client_secret", old)},
	"json":        {200, eventlog.KindSwap, "api", "POST", "/echo", []string{"pass"}, jsonIs("password", pw)},
	"echo-header": {200, eventlog.KindSwap, "api", "GET", "/echo-header", []string{"api-key"}, headerIs("X-Secret", api)},
	"error": {401, eventlog.KindSwap, "api", "POST", "/error", []string{"api-key", "client-secret", "pass"},
		all(headerIs("Authorization", "Bearer "+api), headerIs("X-Api-Key", cs), bodyIs("secret="+pw))},
	"gzip": {200, eventlog.KindSwap, "api", "GET", "/gzip", []string{"api-key"},
		all(headerIs("Authorization", "Bearer "+api), headerAbsent("Accept-Encoding"))},
	"sse":             {200, eventlog.KindSwap, "api", "GET", "/sse", []string{"api-key"}, headerIs("X-Api-Key", api)},
	"other-namespace": {403, eventlog.KindRefuse, "api", "GET", "/echo", nil, nil},
	"failed":          {503, eventlog.KindRefuse, "api", "GET", "/echo", []string{"broken"}, nil},
	"undefined":       {503, eventlog.KindRefuse, "api", "GET", "/echo", []string{"nope"}, nil},
}

func TestProof(t *testing.T) {
	base, records := harness.StartRecorder(t)
	_, state := harness.StartBroker(t, harness.ProofNamespaces(base))
	harness.ClearRecords(t, records)

	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	outcomes := map[string]cases.Outcome{}
	for _, o := range cases.Run(harness.ProofRoutes(), harness.ProofPlaceholders(), client) {
		outcomes[o.Case] = o
	}
	if len(outcomes) != len(expects) {
		t.Fatalf("got %d outcomes, want %d", len(outcomes), len(expects))
	}
	for name, want := range expects {
		o, ok := outcomes[name]
		if !ok {
			t.Fatalf("case %s: no outcome", name)
		}
		if o.Status != want.status {
			t.Errorf("case %s: status %d, want %d (body %q)", name, o.Status, want.status, o.Body)
		}
	}
	byCase := map[string][]harness.Record{}
	for _, r := range harness.Records(t, records) {
		byCase[r.Headers.Get(cases.CaseHeader)] = append(byCase[r.Headers.Get(cases.CaseHeader)], r)
	}

	t.Run("1-records-hold-values", func(t *testing.T) {
		for _, name := range sortedCases() {
			want := expects[name]
			if want.record == nil {
				continue
			}
			recs := byCase[name]
			if len(recs) != 1 {
				t.Errorf("case %s: %d records, want 1", name, len(recs))
				continue
			}
			if err := want.record(recs[0]); err != nil {
				t.Errorf("case %s: record: %v", name, err)
			}
			if where := placeholderIn(recs[0]); where != "" {
				t.Errorf("case %s: the record holds sbxb_t1_ text in its %s", name, where)
			}
		}
	})

	t.Run("2-no-value-in-answers", func(t *testing.T) {
		for _, name := range sortedCases() {
			o := outcomes[name]
			noValue(t, "case "+name+" headers", headerText(o.Headers))
			noValue(t, "case "+name+" body", o.Body)
		}
	})

	t.Run("3-refusals", func(t *testing.T) {
		refusals := []struct {
			name, field, value string
		}{
			{"other-namespace", "namespace", "t2"},
			{"failed", "state", "failed"},
			{"undefined", "state", "undefined"},
		}
		for _, r := range refusals {
			var body map[string]string
			if err := json.Unmarshal([]byte(outcomes[r.name].Body), &body); err != nil {
				t.Errorf("case %s: body %q is not a JSON object: %v", r.name, outcomes[r.name].Body, err)
				continue
			}
			if body[r.field] != r.value {
				t.Errorf("case %s: %s is %q, want %q (body %q)", r.name, r.field, body[r.field], r.value, outcomes[r.name].Body)
			}
			if n := len(byCase[r.name]); n != 0 {
				t.Errorf("case %s: the recorder has %d records, want none", r.name, n)
			}
		}
	})

	t.Run("4-form-token", func(t *testing.T) {
		var tok struct {
			AccessToken string `json:"access_token"`
		}
		if err := json.Unmarshal([]byte(outcomes["form-token"].Body), &tok); err != nil || tok.AccessToken != "tok-1" {
			t.Errorf("form-token: body %q, want access_token tok-1 (%v)", outcomes["form-token"].Body, err)
		}
		rejected := outcomes["form-token-rejected"].Body
		if !strings.Contains(rejected, "sbxb_t1_old_secret") {
			t.Errorf("form-token-rejected: body %q does not quote sbxb_t1_old_secret", rejected)
		}
		noValue(t, "case form-token-rejected body", rejected)
	})

	t.Run("5-sse", func(t *testing.T) {
		if got, want := outcomes["sse"].Body, "data: sbxb_t1_api_key\n\n"; got != want {
			t.Errorf("sse: body %q, want %q", got, want)
		}
	})

	t.Run("6-log", func(t *testing.T) {
		events := waitForCaseEvents(t, state, len(expects))
		var got, want []string
		for _, e := range events {
			got = append(got, eventKey(e.Kind, e.Namespace, e.Route, e.Method, e.Path, e.Status, e.Secrets))
		}
		for _, name := range sortedCases() {
			x := expects[name]
			want = append(want, eventKey(x.kind, "t1", x.route, x.method, x.path, x.status, x.secrets))
		}
		sort.Strings(got)
		sort.Strings(want)
		if !slices.Equal(got, want) {
			t.Errorf("swap and refuse events:\n got  %q\n want %q (one per case)", got, want)
		}
		data, err := os.ReadFile(filepath.Join(state, "broker.log"))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			noValue(t, fmt.Sprintf("broker.log line %d", i+1), line)
		}
	})

	t.Run("7-scrubbed-answers", func(t *testing.T) {
		for _, ph := range []string{"sbxb_t1_api_key", "sbxb_t1_client_secret", "sbxb_t1_pass"} {
			if !strings.Contains(outcomes["error"].Body, ph) {
				t.Errorf("error: body %q does not quote %s", outcomes["error"].Body, ph)
			}
		}
		if got := outcomes["echo-header"].Headers.Get("X-Echo"); got != "sbxb_t1_api_key" {
			t.Errorf("echo-header: X-Echo %q, want sbxb_t1_api_key", got)
		}
		g := outcomes["gzip"]
		if g.Headers.Get("X-Mock-Encoding") != "gzip" {
			t.Errorf("gzip: the recorder did not compress (X-Mock-Encoding %q)", g.Headers.Get("X-Mock-Encoding"))
		}
		if ce := g.Headers.Get("Content-Encoding"); ce != "" {
			t.Errorf("gzip: Content-Encoding %q, want none", ce)
		}
		if !strings.Contains(g.Body, "sbxb_t1_api_key") {
			t.Errorf("gzip: body %q does not hold sbxb_t1_api_key", g.Body)
		}
	})
}

func sortedCases() []string {
	names := make([]string, 0, len(expects))
	for n := range expects {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func eventKey(kind eventlog.Kind, ns, route, method, path string, status int, secrets []string) string {
	s := slices.Clone(secrets)
	sort.Strings(s)
	return fmt.Sprintf("%s %s/%s %s %s %d %v", kind, ns, route, method, path, status, s)
}

// waitForCaseEvents reads broker.log until it holds n swap or refuse events,
// at most 3 s: a swap event is written when the proxy closes the answer,
// which may be a moment after the client read it. It returns them all.
func waitForCaseEvents(t *testing.T, state string, n int) []eventlog.Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		events := caseEvents(t, state)
		if len(events) >= n || time.Now().After(deadline) {
			return events
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func caseEvents(t *testing.T, state string) []eventlog.Event {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(state, "broker.log"))
	if err != nil {
		t.Fatal(err)
	}
	var out []eventlog.Event
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e eventlog.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("broker.log: %v", err)
		}
		if e.Kind == eventlog.KindSwap || e.Kind == eventlog.KindRefuse {
			out = append(out, e)
		}
	}
	return out
}

// placeholderIn names the part of r that holds sbxb_t1_ text, or "".
func placeholderIn(r harness.Record) string {
	const marker = "sbxb_t1_"
	parts := map[string]string{"method": r.Method, "path": r.Path, "rawQuery": r.RawQuery, "headers": headerText(r.Headers), "body": r.Body}
	for _, name := range []string{"method", "path", "rawQuery", "headers", "body"} {
		if strings.Contains(parts[name], marker) {
			return name
		}
	}
	return ""
}

func headerText(h http.Header) string {
	var b strings.Builder
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for k := range h {
			if !yield(k) {
				return
			}
		}
	}) {
		for _, v := range h[name] {
			fmt.Fprintf(&b, "%s: %s\n", name, v)
		}
	}
	return b.String()
}

// The checks of the records.

func headerIs(name, want string) check {
	return func(r harness.Record) error {
		if got := r.Headers.Get(name); got != want {
			return fmt.Errorf("header %s is %q, want the value", name, got)
		}
		return nil
	}
}

func headerAbsent(name string) check {
	return func(r harness.Record) error {
		if got := r.Headers.Values(name); len(got) != 0 {
			return fmt.Errorf("header %s is %q, want none", name, got)
		}
		return nil
	}
}

func base64HeaderIs(name, want string) check {
	return func(r harness.Record) error {
		got, err := base64.StdEncoding.DecodeString(r.Headers.Get(name))
		if err != nil || string(got) != want {
			return fmt.Errorf("header %s decodes to %q (%v), want the value", name, got, err)
		}
		return nil
	}
}

func basicIs(user, pass string) check {
	return func(r harness.Record) error {
		req := &http.Request{Header: r.Headers}
		u, p, ok := req.BasicAuth()
		if !ok || u != user || p != pass {
			return fmt.Errorf("basic auth is %q:%q (%v), want %s and the value", u, p, ok, user)
		}
		return nil
	}
}

func queryIs(key, want string) check {
	return func(r harness.Record) error {
		q, err := url.ParseQuery(r.RawQuery)
		if err != nil || q.Get(key) != want {
			return fmt.Errorf("query %s is %q (%v), want the value", key, q.Get(key), err)
		}
		return nil
	}
}

func pathIs(want string) check {
	return func(r harness.Record) error {
		if r.Path != want {
			return fmt.Errorf("path is %q, want %q", r.Path, want)
		}
		return nil
	}
}

func formIs(key, want string) check {
	return func(r harness.Record) error {
		f, err := url.ParseQuery(r.Body)
		if err != nil || f.Get(key) != want {
			return fmt.Errorf("form field %s is %q (%v), want the value", key, f.Get(key), err)
		}
		return nil
	}
}

func jsonIs(key, want string) check {
	return func(r harness.Record) error {
		var m map[string]string
		if err := json.Unmarshal([]byte(r.Body), &m); err != nil || m[key] != want {
			return fmt.Errorf("JSON field %s is %q (%v), want the value", key, m[key], err)
		}
		return nil
	}
}

func bodyIs(want string) check {
	return func(r harness.Record) error {
		if r.Body != want {
			return fmt.Errorf("body is %q, want %q", r.Body, want)
		}
		return nil
	}
}

func all(checks ...check) check {
	return func(r harness.Record) error {
		var errs []error
		for _, c := range checks {
			errs = append(errs, c(r))
		}
		return errors.Join(errs...)
	}
}

// The value forms (rulings R4, R7, R9).

type form struct{ name, text string }

// forms returns every form of v that must never reach the app or the log:
// raw, the four Base64 encodings, the URL escapes, the JSON escapes, the
// encodeURIComponent form, and the lowercase-hex versions of the percent
// forms.
func forms(v string) []form {
	jsonHTML, _ := json.Marshal(v)
	var plain strings.Builder
	enc := json.NewEncoder(&plain)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	out := []form{
		{"raw", v},
		{"base64 std", base64.StdEncoding.EncodeToString([]byte(v))},
		{"base64 url", base64.URLEncoding.EncodeToString([]byte(v))},
		{"base64 rawstd", base64.RawStdEncoding.EncodeToString([]byte(v))},
		{"base64 rawurl", base64.RawURLEncoding.EncodeToString([]byte(v))},
		{"json html-escaped", strings.Trim(string(jsonHTML), `"`)},
		{"json", strings.Trim(strings.TrimSpace(plain.String()), `"`)},
	}
	for _, p := range []form{
		{"query-escape", url.QueryEscape(v)},
		{"path-escape", url.PathEscape(v)},
		{"encodeURIComponent", encodeURIComponent(v)},
	} {
		out = append(out, p, form{p.name + " lowercase-hex", lowerHex(p.text)})
	}
	return out
}

// encodeURIComponent is JavaScript's: every byte is percent-encoded except
// A-Z a-z 0-9 - _ . ! ~ * ' ( ).
func encodeURIComponent(v string) string {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		c := v[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

var percentHex = regexp.MustCompile(`%[0-9A-F]{2}`)

func lowerHex(s string) string { return percentHex.ReplaceAllStringFunc(s, strings.ToLower) }

var base64Run = regexp.MustCompile(`[A-Za-z0-9+/=_-]{8,}`)

// decodings returns every text a Base64-looking token of s decodes to: each
// token in all four encodings, and also each run between padding at the
// offsets 0 to 3 in both alphabets, so a value inside a longer token (ruling
// R4) is found too.
func decodings(s string) []string {
	var out []string
	encs := []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding}
	for _, tok := range base64Run.FindAllString(s, -1) {
		for _, enc := range encs {
			if d, err := enc.DecodeString(tok); err == nil {
				out = append(out, string(d))
			}
		}
		for _, piece := range strings.FieldsFunc(tok, func(r rune) bool { return r == '=' }) {
			for off := 0; off < 4 && len(piece)-off >= 8; off++ {
				p := piece[off:]
				if len(p)%4 == 1 {
					p = p[:len(p)-1]
				}
				for _, enc := range []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding} {
					if d, err := enc.DecodeString(p); err == nil {
						out = append(out, string(d))
					}
				}
			}
		}
	}
	return out
}

// noValue fails when text holds any form of any proof value, or a Base64
// token of it decodes to text that holds a value.
func noValue(t *testing.T, where, text string) {
	t.Helper()
	values := harness.ProofValues()
	labels := make([]string, 0, len(values))
	for l := range values {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	decoded := decodings(text)
	for _, label := range labels {
		v := values[label]
		for _, f := range forms(v) {
			if strings.Contains(text, f.text) {
				t.Errorf("%s holds the value %s (%s) in the form %s", where, label, v, f.name)
			}
		}
		for _, d := range decoded {
			if strings.Contains(d, v) {
				t.Errorf("%s holds the value %s (%s) inside a Base64 token", where, label, v)
				break
			}
		}
	}
}
