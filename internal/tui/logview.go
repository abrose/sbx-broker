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
	logKeep  = 1000                   // the newest events the Log view keeps
	logEvery = 250 * time.Millisecond // how often the model takes the events the follow collected
)

// Messages of the Log view.
type (
	followMsg  struct{} // a follow was started; the tick chain begins
	logTickMsg struct{} // take the events the follow collected
)

// logFeed hands events from the follow goroutine to the model: the goroutine
// adds, the model takes at each tick. No client call runs in Update, and the
// goroutine needs no reference to the Bubble Tea program.
type logFeed struct {
	mu     sync.Mutex
	events []eventlog.Event
	ended  bool // Follow returned: the connection dropped or the context ended
	err    error
}

func (f *logFeed) add(e eventlog.Event) {
	f.mu.Lock()
	f.events = append(f.events, e)
	f.mu.Unlock()
}

func (f *logFeed) end(err error) {
	f.mu.Lock()
	f.ended, f.err = true, err
	f.mu.Unlock()
}

func (f *logFeed) take() (events []eventlog.Event, ended bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	events, f.events = f.events, nil
	return events, f.ended, f.err
}

// logView is the state of the Log view.
type logView struct {
	events   []eventlog.Event // the newest logKeep, oldest first
	nsFilter string           // "" shows all
	kind     eventlog.Kind    // "" shows all
	scroll   int              // lines above the newest; 0 follows the newest
	mark     time.Time        // the newest time held when the current follow started
	err      string           // why the last follow ended, printable; "" when none or expected

	following bool // one follow runs at a time
	feed      *logFeed
	ctx       context.Context
	cancel    context.CancelFunc
}

// followCmd starts the one follow, unless one runs. The goroutine ends when
// the connection drops or stopFollow cancels its context; the tick finds out.
func (m *Model) followCmd() tea.Cmd {
	if m.log.following {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	feed := &logFeed{}
	m.log.following, m.log.feed, m.log.ctx, m.log.cancel = true, feed, ctx, cancel
	m.log.mark = time.Time{}
	if n := len(m.log.events); n > 0 {
		for _, e := range m.log.events {
			if e.Time.After(m.log.mark) {
				m.log.mark = e.Time
			}
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

// logTick takes the collected events and, while the follow lives, asks for
// the next tick.
func (m *Model) logTick() tea.Cmd {
	l := &m.log
	if l.feed == nil {
		return nil
	}
	events, ended, err := l.feed.take()
	for _, e := range events {
		m.addEvent(e)
	}
	if len(events) > 0 {
		l.err = "" // a follow that delivers is working
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
	return m.after(logEvery, logTickMsg{})
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
		for i := len(l.events) - 1; i >= 0; i-- {
			if l.events[i].Time.Equal(e.Time) && eventlog.Line(l.events[i]) == line {
				return
			}
		}
	}
	l.events = append(l.events, e)
	if len(l.events) > logKeep {
		l.events = append(l.events[:0], l.events[len(l.events)-logKeep:]...)
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

// logRows is how many lines the view shows: the screen less the header, the
// view's own head and the footer. 0 means all of them (the size is unknown).
func (m *Model) logRows() int {
	if m.height == 0 {
		return 0
	}
	return max(m.height-strings.Count(m.header(), "\n")-1-5, 3)
}

func (m *Model) logLines() []string {
	var lines []string
	for _, e := range m.log.events {
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
	rows := m.logRows()
	if rows == 0 {
		return 0
	}
	return max(len(m.logLines())-rows, 0)
}

func (m *Model) logBody() string {
	l := m.log
	all := func(s string) string {
		if s == "" {
			return "all"
		}
		return s
	}
	head := fmt.Sprintf("log · ns %s · kind %s", p(all(l.nsFilter)), all(string(l.kind)))
	if l.err != "" {
		head += "\nfollow: " + l.err
	}
	lines := m.logLines()
	if len(lines) == 0 {
		return head + "\n\nno events"
	}
	rows := m.logRows()
	if rows == 0 {
		rows = len(lines)
	}
	end := len(lines) - min(l.scroll, max(len(lines)-rows, 0))
	return head + "\n\n" + strings.Join(lines[max(end-rows, 0):end], "\n")
}
