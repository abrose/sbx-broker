package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/abrose/sbx-broker/internal/admin"
	"github.com/abrose/sbx-broker/internal/eventlog"
)

const (
	logKeep = 1000 // the newest events the Log view keeps; it holds up to twice as many between two trims
	logGap  = 1    // the blank line between the Log view's head and its events
)

// Messages of the Log view.
type (
	followMsg  struct{} // a follow was started: wait for its first wake
	logTickMsg struct{} // the follow woke the model: take what it collected
)

// logFeed hands events from the follow goroutine to the model: the goroutine
// adds and wakes the model, the model takes when woken. No client call runs
// in Update, and the goroutine needs no reference to the Bubble Tea program.
type logFeed struct {
	// wake holds one wake at most: it stands for everything added or ended
	// since the last take, as the model takes it all at once.
	wake chan struct{}

	mu     sync.Mutex
	events []eventlog.Event
	ended  bool // Follow returned: the connection dropped or the context ended
	err    error
}

func newLogFeed() *logFeed { return &logFeed{wake: make(chan struct{}, 1)} }

func (f *logFeed) add(e eventlog.Event) {
	f.mu.Lock()
	f.events = append(f.events, e)
	f.mu.Unlock()
	f.signal()
}

func (f *logFeed) end(err error) {
	f.mu.Lock()
	f.ended, f.err = true, err
	f.mu.Unlock()
	f.signal()
}

// signal wakes the model without blocking: a wake that waits already covers
// what came since, because the model takes after the wake.
func (f *logFeed) signal() {
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func (f *logFeed) take() (events []eventlog.Event, ended bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	events, f.events = f.events, nil
	return events, f.ended, f.err
}

// woken is the command that blocks until the follow adds an event or ends.
// The end of the follow wakes it, so it ends with the follow, at quit too.
func (f *logFeed) woken() tea.Cmd {
	return func() tea.Msg {
		<-f.wake
		return logTickMsg{}
	}
}

// logView is the state of the Log view.
type logView struct {
	events   []eventlog.Event // oldest first; kept() is what the view shows
	nsFilter string           // "" shows all
	kind     eventlog.Kind    // "" shows all
	scroll   int              // lines above the newest; 0 follows the newest
	mark     time.Time        // the newest time held when the current follow started
	err      string           // why the last follow ended, printable; "" when none or expected

	following bool // one follow runs at a time
	feed      *logFeed
	cancel    context.CancelFunc
}

// kept returns the newest logKeep events, oldest first.
func (l *logView) kept() []eventlog.Event {
	return l.events[max(len(l.events)-logKeep, 0):]
}

// followCmd starts the one follow, unless one runs. The goroutine ends when
// the connection drops or stopFollow cancels its context; its end wakes the
// model.
func (m *Model) followCmd() tea.Cmd {
	l := &m.log
	if l.following {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	feed := newLogFeed()
	l.following, l.feed, l.cancel, l.err = true, feed, cancel, ""
	l.mark = time.Time{}
	for _, e := range l.kept() {
		if e.Time.After(l.mark) {
			l.mark = e.Time
		}
	}
	cl := m.cl
	return func() tea.Msg {
		go func() {
			// A drop or an error ends the feed; the next good status follows again.
			feed.end(cl.Follow(ctx, feed.add))
		}()
		return followMsg{}
	}
}

// stopFollow cancels the follow, if any. The TUI calls it when it quits.
func (m *Model) stopFollow() {
	if m.log.cancel != nil {
		m.log.cancel()
	}
}

// waitLog waits for the next wake of the follow, while one runs.
func (m *Model) waitLog() tea.Cmd {
	if m.log.feed == nil {
		return nil
	}
	return m.wait(m.log.feed.woken())
}

// logTick takes the collected events and, while the follow lives, waits for
// the next wake.
func (m *Model) logTick() tea.Cmd {
	l := &m.log
	if l.feed == nil {
		return nil
	}
	events, ended, err := l.feed.take()
	for _, e := range events {
		m.addEvent(e)
	}
	if len(events) > 0 && m.height > 0 {
		l.scroll = min(l.scroll, m.logMaxScroll()) // the events trimmed from the top leave less to scroll
	}
	if ended {
		// No broker and a cancel are expected; anything else the owner should see.
		if err != nil && !errors.Is(err, admin.ErrNoBroker) && !errors.Is(err, context.Canceled) {
			l.err = eventlog.Printable(err.Error())
		}
		l.cancel()
		l.following, l.feed = false, nil
		return nil
	}
	return m.waitLog()
}

// addEvent appends e unless it is a replay: a follow that starts again gets
// the broker's last 100 events first. Only an event at or before the newest
// time held when the follow started can be a replay, and only when an
// identical (time, line) is held. The broker does not deliver live events in
// time order, so a live event is never dropped for being old.
func (m *Model) addEvent(e eventlog.Event) {
	l := &m.log
	if !e.Time.After(l.mark) {
		line := eventlog.Line(e)
		kept := l.kept()
		for i := len(kept) - 1; i >= 0; i-- {
			if kept[i].Time.Equal(e.Time) && eventlog.Line(kept[i]) == line {
				return
			}
		}
	}
	l.events = append(l.events, e)
	if len(l.events) > 2*logKeep {
		l.events = l.events[:copy(l.events, l.kept())] // trim: one copy for every logKeep events added
	}
	if l.scroll > 0 && m.logMatches(e) {
		l.scroll++ // the view stays where the owner scrolled it
	}
}

func (m *Model) logMatches(e eventlog.Event) bool {
	return (m.log.nsFilter == "" || e.Namespace == m.log.nsFilter) && (m.log.kind == "" || e.Kind == m.log.kind)
}

func (m *Model) logKey(k tea.KeyPressMsg) {
	l := &m.log
	switch {
	case key.Matches(k, m.keys.nsFilter):
		names := []string{""}
		for _, ns := range m.nss {
			names = append(names, ns.Name)
		}
		l.nsFilter, l.scroll = nextOf(names, l.nsFilter), 0
	case key.Matches(k, m.keys.kindFilter):
		kinds := append([]eventlog.Kind{""}, eventlog.Kinds...)
		l.kind, l.scroll = nextOf(kinds, l.kind), 0
	case key.Matches(k, m.keys.clear):
		l.nsFilter, l.kind, l.scroll = "", "", 0
	case key.Matches(k, m.keys.up):
		l.scroll = min(l.scroll+1, m.logMaxScroll())
	case key.Matches(k, m.keys.down):
		l.scroll = max(l.scroll-1, 0)
	case key.Matches(k, m.keys.newest):
		l.scroll = 0
	}
}

// nextOf returns the item after cur in items, wrapping; the first item when
// cur is not in items (a namespace that went away).
func nextOf[T comparable](items []T, cur T) T {
	for i, it := range items {
		if it == cur {
			return items[(i+1)%len(items)]
		}
	}
	return items[0]
}

// logHead is the head of the Log view: its filter line and, after a failed
// follow, the error line, each cut to the width. n is its line count.
func (m *Model) logHead() (head string, n int) {
	l := m.log
	all := func(s string) string {
		if s == "" {
			return "all"
		}
		return s
	}
	lines := []string{fmt.Sprintf("log · ns %s · kind %s", p(all(l.nsFilter)), all(string(l.kind)))}
	if l.err != "" {
		lines = append(lines, "follow: "+l.err)
	}
	if m.width > 0 {
		for i := range lines {
			lines[i] = cut(lines[i], m.width)
		}
	}
	return strings.Join(lines, "\n"), len(lines)
}

// logRows is how many event lines the view shows: the screen less the
// header, render's blank lines and footer, the view's head and the gap under
// it; 0 when they leave no room. ok is false while the size is unknown: then
// all of them show.
func (m *Model) logRows() (rows int, ok bool) {
	if m.height == 0 {
		return 0, false
	}
	header := strings.Count(m.header(), "\n") + 1
	_, head := m.logHead()
	return max(m.height-header-renderBlanks-footerLines-head-logGap, 0), true
}

func (m *Model) logLines() []string {
	var lines []string
	for _, e := range m.log.kept() {
		if m.logMatches(e) {
			line := eventlog.Line(e)
			if m.width > 0 {
				line = cut(line, m.width)
			}
			lines = append(lines, line)
		}
	}
	return lines
}

func (m *Model) logMaxScroll() int {
	rows, ok := m.logRows()
	if !ok {
		return 0
	}
	return max(len(m.logLines())-rows, 0)
}

func (m *Model) logBody() string {
	head, _ := m.logHead()
	lines := m.logLines()
	rows, ok := m.logRows()
	switch {
	case ok && rows == 0:
		return head // no room for an event line
	case len(lines) == 0:
		return head + "\n\nno events"
	case !ok:
		rows = len(lines)
	}
	end := len(lines) - min(m.log.scroll, max(len(lines)-rows, 0))
	return head + "\n\n" + strings.Join(lines[max(end-rows, 0):end], "\n")
}
