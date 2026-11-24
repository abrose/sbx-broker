package tui

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

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

// wake runs the last command that waits for the follow, as the runtime would
// in its own goroutine, and sends its message.
func (h *harness) wake() {
	h.t.Helper()
	if len(h.waits) == 0 {
		h.t.Fatal("no command waits for the follow")
	}
	got := make(chan tea.Msg, 1)
	go func(c tea.Cmd) { got <- c() }(h.waits[len(h.waits)-1])
	select {
	case msg := <-got:
		h.send(msg)
	case <-time.After(3 * time.Second):
		h.t.Fatal("the follow never woke the model")
	}
}

// noLogTimer fails when a timer other than the status poll is armed.
func (h *harness) noLogTimer() {
	h.t.Helper()
	for _, tm := range h.timers {
		if tm.d != pollEvery {
			h.t.Fatalf("a timer other than the status poll: %v", h.timers)
		}
	}
}

// waitFor fails unless cond holds within 3 s; it sends nothing.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); !cond(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s never happened", what)
		}
	}
}

func eventsFrom(first, last int) []eventlog.Event {
	var events []eventlog.Event
	for i := first; i <= last; i++ {
		events = append(events, ev(eventlog.KindSwap, "acme", i))
	}
	return events
}

func paths(events []eventlog.Event) []string {
	var ps []string
	for _, e := range events {
		ps = append(ps, e.Path)
	}
	return ps
}

func TestLogKeepsNewest1000(t *testing.T) {
	h := startLog(t, followStep{events: eventsFrom(1, 1500)})
	h.logScreen("/e1500")
	if got, want := paths(h.m.log.kept()), paths(eventsFrom(501, 1500)); !slices.Equal(got, want) {
		t.Fatalf("kept %d events from %v to %v, want 501 to 1500", len(got), got[0], got[len(got)-1])
	}
	if n := len(h.m.log.events); n > 2*logKeep {
		t.Fatalf("holds %d events, more than %d", n, 2*logKeep)
	}
	h.has("/e0501", "/e1500")
	h.hasNot("/e0500", "/e0001")
	// Past 2*logKeep the view drops the older half in one copy.
	for _, e := range eventsFrom(1501, 2500) {
		h.m.log.feed.add(e)
	}
	h.logScreen("/e2500")
	if got, want := paths(h.m.log.kept()), paths(eventsFrom(1501, 2500)); !slices.Equal(got, want) {
		t.Fatalf("kept %d events from %v to %v, want 1501 to 2500", len(got), got[0], got[len(got)-1])
	}
	if n := len(h.m.log.events); n > 2*logKeep {
		t.Fatalf("holds %d events, more than %d", n, 2*logKeep)
	}
}

// The screen never holds more lines than the terminal has rows, nor a line
// wider than the terminal; a follow error takes a row from the events.
func TestLogFitsScreen(t *testing.T) {
	const width = 40
	for _, height := range []int{6, 12, 40} {
		for _, followErr := range []error{nil, errors.New(strings.Repeat("x", 80))} {
			t.Run(fmt.Sprintf("height %d error %v", height, followErr != nil), func(t *testing.T) {
				h := startLog(t, followStep{events: eventsFrom(1, 100), err: followErr})
				h.send(tea.WindowSizeMsg{Width: width, Height: height})
				h.until(func() bool { return len(h.m.log.kept()) == 100 && (followErr == nil || !h.m.log.following) })
				if followErr != nil {
					h.has("follow: xxx")
				}
				s := h.screen()
				lines := strings.Split(s, "\n")
				if len(lines) > height {
					t.Fatalf("%d lines on %d rows:\n%s", len(lines), height, s)
				}
				if height == 40 && len(lines) != height {
					t.Fatalf("%d lines on %d rows, want every row used:\n%s", len(lines), height, s)
				}
				for _, l := range lines {
					if lipgloss.Width(l) > width {
						t.Fatalf("a line of %d cells on %d columns: %q", lipgloss.Width(l), width, l)
					}
				}
			})
		}
	}
}

func TestLogErrorClearsOnNewFollow(t *testing.T) {
	h := startLog(t, followStep{err: errors.New("bad request")})
	h.logScreen("follow: bad request")
	h.send(h.lastTimer(pollEvery).msg) // the next status starts a follow that has no event yet
	if !h.m.log.following {
		t.Fatal("no new follow started")
	}
	h.hasNot("follow:")
}

// An idle follow arms no timer: the model hears of new events from the follow.
func TestLogDoesNotPollWhenIdle(t *testing.T) {
	h := startLog(t)
	waitFor(t, "the follow", func() bool { return h.fake.followCount() == 1 })
	h.noLogTimer()
	if len(h.timers) == 0 {
		t.Fatal("no status poll armed")
	}
}

func TestLogShowsEventsWithoutTimer(t *testing.T) {
	h := startLog(t, followStep{events: []eventlog.Event{ev(eventlog.KindSwap, "acme", 1)}})
	h.wake()
	h.has("/e0001")
	h.noLogTimer()
	if len(h.waits) != 2 {
		t.Fatalf("%d waits, want the first and the one armed again while the follow lives", len(h.waits))
	}
}

// Old events that the view drops do not leave the scroll past its end: the
// next down moves the view.
func TestLogScrollStaysWithinMax(t *testing.T) {
	h := startLog(t, followStep{events: eventsFrom(1, 50)})
	h.send(tea.WindowSizeMsg{Width: 100, Height: 20})
	h.logScreen("/e0050")
	h.keys("up", "up", "up")
	for _, e := range eventsFrom(51, 1050) {
		h.m.log.feed.add(e)
	}
	h.until(func() bool { k := h.m.log.kept(); return k[len(k)-1].Path == "/e1050" })
	before := h.screen()
	h.keys("down")
	if h.screen() == before {
		t.Fatalf("down did not move the view:\n%s", before)
	}
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
	if h.m.followCmd() != nil {
		t.Fatal("a second follow while one runs")
	}
	h.keys("q")
	waitFor(t, "the end of the follow at quit", func() bool { return h.fake.followEnded() == 1 })
	h.wake() // the command that waits for the follow ends too
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
