package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/abrose/sbx-broker/internal/admin"
	"github.com/abrose/sbx-broker/internal/eventlog"
)

var logBase = time.Date(2026, 11, 23, 10, 0, 0, 0, time.Local)

// ev is event number i: its path /e<i> tells lines apart.
func ev(kind eventlog.Kind, ns string, i int) eventlog.Event {
	return eventlog.Event{Time: logBase.Add(time.Duration(i) * time.Second), Kind: kind, Namespace: ns, Route: "api",
		Method: "GET", Path: fmt.Sprintf("/e%04d", i)}
}

// until sends the log tick until cond holds: the follow goroutine hands its
// events over at a tick.
func (h *harness) until(cond func() bool) {
	h.t.Helper()
	for deadline := time.Now().Add(3 * time.Second); !cond(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			h.t.Fatalf("the condition never held:\n%s", h.screen())
		}
		h.send(logTickMsg{})
	}
}

func (h *harness) logScreen(want string) {
	h.t.Helper()
	h.until(func() bool { return strings.Contains(h.screen(), want) })
}

func startLog(t *testing.T, steps ...followStep) *harness {
	t.Helper()
	cl := &fakeClient{nss: fixture(), script: steps}
	h := newHarness(t, cl, Options{})
	h.keys("4")
	return h
}

func TestLogShowsFollowedEvents(t *testing.T) {
	h := startLog(t, followStep{events: []eventlog.Event{ev(eventlog.KindSwap, "acme", 1), ev(eventlog.KindRefuse, "acme", 2),
		ev(eventlog.KindFetch, "beta", 3)}})
	h.logScreen("/e0003")
	h.has("log · ns all · kind all", "10:00:01 swap acme api GET /e0001", "10:00:02 refuse acme", "10:00:03 fetch beta")
	s := h.screen()
	if !(strings.Index(s, "/e0001") < strings.Index(s, "/e0002") && strings.Index(s, "/e0002") < strings.Index(s, "/e0003")) {
		t.Fatalf("newest is not last:\n%s", s)
	}
	if h.fake.followCount() != 1 {
		t.Fatalf("follows %d", h.fake.followCount())
	}
}

func TestLogKeepsNewest1000(t *testing.T) {
	var events []eventlog.Event
	for i := 1; i <= 1005; i++ {
		events = append(events, ev(eventlog.KindSwap, "acme", i))
	}
	h := startLog(t, followStep{events: events})
	h.logScreen("/e1005")
	if n := len(h.m.log.events); n != 1000 {
		t.Fatalf("kept %d events", n)
	}
	h.has("/e0006", "/e1005")
	h.hasNot("/e0005", "/e0001")
}

func TestLogFilters(t *testing.T) {
	h := startLog(t, followStep{events: []eventlog.Event{
		ev(eventlog.KindSwap, "acme", 1), ev(eventlog.KindRefuse, "acme", 2),
		ev(eventlog.KindRefuse, "beta", 3), ev(eventlog.KindSwap, "beta", 4)}})
	h.logScreen("/e0004")
	h.keys("n") // the namespaces come in order: acme first
	h.has("ns acme · kind all", "/e0001", "/e0002")
	h.hasNot("/e0003", "/e0004")
	h.keys("t", "t") // swap, then refuse
	h.has("ns acme · kind refuse", "/e0002")
	h.hasNot("/e0001", "/e0003")
	h.keys("c")
	h.has("ns all · kind all", "/e0001", "/e0002", "/e0003", "/e0004")
	// n goes through every namespace and back to all.
	h.keys("n", "n", "n")
	h.has("ns all · kind all")
}

func TestLogScrollsAndGoesToNewest(t *testing.T) {
	var events []eventlog.Event
	for i := 1; i <= 50; i++ {
		events = append(events, ev(eventlog.KindSwap, "acme", i))
	}
	h := startLog(t, followStep{events: events})
	h.send(tea.WindowSizeMsg{Width: 100, Height: 20})
	h.logScreen("/e0050")
	h.hasNot("/e0001")
	h.keys("up", "up", "up")
	h.hasNot("/e0050")
	h.has("/e0047")
	h.keys("G")
	h.has("/e0050")
	h.keys("down") // not below the newest
	h.has("/e0050")
}

func TestLogFollowsAgainAfterBrokerRestart(t *testing.T) {
	h := startLog(t,
		followStep{events: []eventlog.Event{ev(eventlog.KindSwap, "acme", 1)}, err: errors.New("connection dropped")},
		followStep{events: []eventlog.Event{ev(eventlog.KindSwap, "acme", 1), ev(eventlog.KindSwap, "acme", 2)}})
	h.logScreen("/e0001")
	// The drop is seen at a tick; no new follow starts while the broker does not answer.
	h.until(func() bool { return !h.m.log.following })
	h.fake.statusErr = admin.ErrNoBroker
	h.send(h.lastTimer(pollEvery).msg)
	if h.fake.followCount() != 1 {
		t.Fatalf("follows %d while no broker answers", h.fake.followCount())
	}
	h.fake.statusErr = nil
	h.send(h.lastTimer(pollEvery).msg)
	h.logScreen("/e0002")
	if h.fake.followCount() != 2 {
		t.Fatalf("follows %d, want 2", h.fake.followCount())
	}
	if strings.Count(h.screen(), "/e0001") != 1 {
		t.Fatalf("the replayed event shows twice:\n%s", h.screen())
	}
}

// The broker does not deliver events in time order (concurrent Adds); a live
// event older than the newest one is still shown.
func TestLogKeepsOutOfOrderLiveEvents(t *testing.T) {
	h := startLog(t, followStep{events: []eventlog.Event{ev(eventlog.KindSwap, "acme", 2), ev(eventlog.KindRefuse, "acme", 1)}})
	h.logScreen("/e0001")
	h.has("/e0002", "/e0001")
}

func TestLogShowsFollowError(t *testing.T) {
	h := startLog(t, followStep{err: errors.New("bad request\x1b[2J")})
	h.logScreen(`follow: bad request\x1b[2J`)
	h.hasNot("\x1b[2J")
	// A new follow that delivers events clears it.
	h.fake.mu.Lock()
	h.fake.script = []followStep{{events: []eventlog.Event{ev(eventlog.KindSwap, "acme", 1)}}}
	h.fake.mu.Unlock()
	h.send(h.lastTimer(pollEvery).msg)
	h.logScreen("/e0001")
	h.hasNot("follow:")
}

func TestLogOnlyOneFollowAndQuitCancels(t *testing.T) {
	h := startLog(t)
	for range 3 {
		h.send(h.lastTimer(pollEvery).msg)
	}
	for deadline := time.Now().Add(3 * time.Second); h.fake.followCount() == 0; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no follow started")
		}
	}
	h.send(h.lastTimer(pollEvery).msg)
	if h.fake.followCount() != 1 {
		t.Fatalf("follows %d, want 1", h.fake.followCount())
	}
	ctx := h.m.log.ctx
	h.keys("q")
	if ctx.Err() == nil {
		t.Fatal("quit left the follow running")
	}
}

func TestLogEscapesEventText(t *testing.T) {
	e := ev(eventlog.KindSwap, "acme", 1)
	e.Path, e.Message = "/a\x1b[2J", "x\ny"
	h := startLog(t, followStep{events: []eventlog.Event{e}})
	h.logScreen(`/a\x1b[2J`)
	h.has(`x\x0ay`)
	if strings.Contains(h.screen(), "\x1b[2J") {
		t.Fatal("a control sequence reaches the screen")
	}
}

func TestLogViewHidesRevealedValue(t *testing.T) {
	h := revealed(t)
	h.keys("4")
	h.wiped()
	h.has("log · ns all · kind all")
}
