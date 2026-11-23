//go:build integration

// Package integration proves the broker end to end: the broker of
// internal/server runs on this host, the recorder mock runs in a container,
// and the cases of test/cases go through the broker's routes to it.
package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/abrose/sbx-broker/internal/eventlog"
	"github.com/abrose/sbx-broker/test/cases"
	"github.com/abrose/sbx-broker/test/harness"
	"github.com/abrose/sbx-broker/test/pgcases"
)

func TestProof(t *testing.T) {
	base, records := harness.StartRecorder(t)
	pgUpstream, pgLogs := harness.StartPostgres(t)
	_, state := harness.StartBroker(t, harness.ProofNamespaces(base, pgUpstream))
	harness.ClearRecords(t, records)

	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	outcomes := harness.OutcomesByCase(t, cases.Run(harness.ProofRoutes(), harness.ProofPlaceholders(), client))
	harness.AssertProof(t, outcomes, harness.RecordsByCase(harness.Records(t, records)))

	t.Run("6-log", func(t *testing.T) {
		events := waitForCaseEvents(t, state, len(harness.Expects))
		var got, want []string
		for _, e := range events {
			got = append(got, eventKey(e.Kind, e.Namespace, e.Route, e.Method, e.Path, e.Status, e.Secrets))
		}
		for _, name := range harness.SortedCases() {
			x := harness.Expects[name]
			want = append(want, eventKey(x.Kind, "t1", x.Route, x.Method, x.Path, x.Status, x.Secrets))
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
			harness.NoValue(t, fmt.Sprintf("broker.log line %d", i+1), line)
		}
	})

	t.Run("8-postgres", func(t *testing.T) { pgProof(t, state, pgLogs) })

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

// pgProof is assertion 8: the Postgres cases, each with its one event.
func pgProof(t *testing.T, state string, pgLogs func() string) {
	outcomes := pgcases.Run(fmt.Sprintf("127.0.0.1:%d", harness.PortT1PG), harness.ProofPlaceholders())
	harness.AssertPostgres(t, outcomes, harness.WaitUpstreamLog(pgLogs))

	want := harness.PGWantEvents()
	got := waitForPGEvents(t, state, len(want))
	if !slices.Equal(got, want) {
		t.Errorf("postgres events:\n got  %q\n want %q (one per case)", got, want)
	}
	data, err := os.ReadFile(filepath.Join(state, "broker.log"))
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		harness.NoValue(t, fmt.Sprintf("broker.log line %d", i+1), line)
	}
}

// waitForPGEvents reads broker.log until it holds n Postgres swap, refuse or
// upstream events, at most 3 s, and returns their keys, sorted. A swap event
// is written at the hand-off, which may be a moment after the app got its
// answer.
func waitForPGEvents(t *testing.T, state string, n int) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var keys []string
		for _, e := range caseEvents(t, state) {
			if harness.IsPGEvent(e) {
				keys = append(keys, harness.PGEventKey(e))
			}
		}
		if len(keys) >= n || time.Now().After(deadline) {
			sort.Strings(keys)
			return keys
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func eventKey(kind eventlog.Kind, ns, route, method, path string, status int, secrets []string) string {
	s := slices.Clone(secrets)
	sort.Strings(s)
	return fmt.Sprintf("%s %s/%s %s %s %d %v", kind, ns, route, method, path, status, s)
}

// waitForCaseEvents reads broker.log until it holds n swap or refuse events
// of the HTTP routes, at most 3 s: a swap event is written when the proxy closes the answer,
// which may be a moment after the client read it. It returns them all.
func waitForCaseEvents(t *testing.T, state string, n int) []eventlog.Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		events := httpEvents(caseEvents(t, state))
		if len(events) >= n || time.Now().After(deadline) {
			return events
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// httpEvents keeps the events of the HTTP routes api and idp.
func httpEvents(events []eventlog.Event) []eventlog.Event {
	var out []eventlog.Event
	for _, e := range events {
		if e.User == "" && (e.Kind == eventlog.KindSwap || e.Kind == eventlog.KindRefuse) && (e.Route == "api" || e.Route == "idp") {
			out = append(out, e)
		}
	}
	return out
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
		if e.Kind == eventlog.KindSwap || e.Kind == eventlog.KindRefuse || e.Kind == eventlog.KindUpstream || e.Kind == eventlog.KindViolation {
			out = append(out, e)
		}
	}
	return out
}
