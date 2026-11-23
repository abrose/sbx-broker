package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

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
}

func (f *logFeed) add(e eventlog.Event) {
	f.mu.Lock()
	f.events = append(f.events, e)
	f.mu.Unlock()
}

func (f *logFeed) end() {
	f.mu.Lock()
	f.ended = true
	f.mu.Unlock()
}

func (f *logFeed) take() (events []eventlog.Event, ended bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	events, f.events = f.events, nil
	return events, f.ended
}

// logView is the state of the Log view.
type logView struct {
	events   []eventlog.Event // the newest logKeep, oldest first
	nsFilter string           // "" shows all
	kind     eventlog.Kind    // "" shows all
	scroll   int              // lines above the newest; 0 follows the newest

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
	cl := m.cl
	return func() tea.Msg {
		go func() {
			_ = cl.Follow(ctx, feed.add) // an error or a drop ends the feed; the next good status follows again
			feed.end()
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
	events, ended := l.feed.take()
	for _, e := range events {
		m.addEvent(e)
	}
	if ended {
		l.cancel()
		l.following, l.feed = false, nil
		return nil
	}
	return m.after(logEvery, logTickMsg{})
}

// addEvent appends e unless the log has it already: a follow that starts
// again replays the broker's last 100 events.
func (m *Model) addEvent(e eventlog.Event) {
	l := &m.log
	line := eventlog.Line(e)
	for i := len(l.events) - 1; i >= 0 && !l.events[i].Time.Before(e.Time); i-- {
		if l.events[i].Time.Equal(e.Time) && eventlog.Line(l.events[i]) == line {
			return
		}
	}
	if n := len(l.events); n > 0 && e.Time.Before(l.events[n-1].Time) {
		return // older than the newest: a replay
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
