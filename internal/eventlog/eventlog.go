// Package eventlog keeps the broker's events: a memory ring for the CLI and
// the TUI, and JSON lines in a size-rotated file. No event field holds a
// secret value; the redactor is a second line of defense for free text.
package eventlog

import (
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"
)

// Kind names what happened.
type Kind string

const (
	KindSwap       Kind = "swap"
	KindRefuse     Kind = "refuse"
	KindFetch      Kind = "fetch"
	KindReload     Kind = "reload"
	KindViolation  Kind = "violation"
	KindUnscanned  Kind = "unscanned"
	KindUpstream   Kind = "upstream"
	KindError      Kind = "error"
	KindInfo       Kind = "info"
	KindClose      Kind = "close"  // a Postgres session ended
	KindReveal     Kind = "reveal" // the owner showed a value through the admin socket
	subscriberSize      = 256
	fileMode            = 0o600
)

// Event is one thing the broker did. Secrets holds names only.
type Event struct {
	Time       time.Time `json:"time"`
	Kind       Kind      `json:"kind"`
	Namespace  string    `json:"ns,omitempty"`
	Route      string    `json:"route,omitempty"`
	User       string    `json:"user,omitempty"` // the app's Postgres user
	Method     string    `json:"method,omitempty"`
	Path       string    `json:"path,omitempty"` // never the query
	Secrets    []string  `json:"secrets,omitempty"`
	Direction  string    `json:"dir,omitempty"` // "request" or "response"
	Status     int       `json:"status,omitempty"`
	DurationMS int64     `json:"ms,omitempty"`
	Message    string    `json:"msg,omitempty"`
}

// Log is safe for concurrent use.
type Log struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	ringCap  int
	ring     []Event
	redact   func(string) string
	file     *os.File
	size     int64
	fileErr  error // first file error; returned by Close
	subs     map[int]chan Event
	nextSub  int
	swaps    map[string]map[string]int // swap events since Open: namespace, secret
}

// Open opens (or creates) the log file in append mode with mode 0600. The
// ring keeps the last ring events; the file rotates to path+".1" before a
// write that would pass maxBytes.
func Open(path string, ring int, maxBytes int64) (*Log, error) {
	l := &Log{path: path, maxBytes: maxBytes, ringCap: ring, subs: map[int]chan Event{}, swaps: map[string]map[string]int{}}
	if err := l.openFile(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Log) openFile() error {
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, fileMode)
	if err != nil {
		return err
	}
	// The mode argument of OpenFile applies on creation only.
	if err := f.Chmod(fileMode); err != nil {
		_ = f.Close()
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	l.file, l.size = f, info.Size()
	return nil
}

// SetRedactor sets the function applied to Message, Path, Route and User
// inside Add.
func (l *Log) SetRedactor(f func(string) string) {
	l.mu.Lock()
	l.redact = f
	l.mu.Unlock()
}

// Add records e. It sets Time when zero, redacts Message, Path, Route and User, then
// stores the event in the ring, appends it to the file and offers it to
// every subscriber. It never blocks and never fails: a file error is
// remembered and returned by Close.
func (l *Log) Add(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.redact != nil {
		e.Message = l.redact(e.Message)
		e.Path = l.redact(e.Path)
		e.Route = l.redact(e.Route)
		e.User = l.redact(e.User)
	}
	if e.Kind == KindSwap {
		per := l.swaps[e.Namespace]
		if per == nil {
			per = map[string]int{}
			l.swaps[e.Namespace] = per
		}
		for _, s := range e.Secrets {
			per[s]++
		}
	}
	l.ring = append(l.ring, e)
	if len(l.ring) >= 2*l.ringCap { // compact lazily: amortized O(1) per Add
		l.ring = append(l.ring[:0], l.ring[len(l.ring)-l.ringCap:]...)
	}
	l.writeLine(e)
	for _, ch := range l.subs {
		select {
		case ch <- e:
		default: // full: drop for this subscriber
		}
	}
}

func (l *Log) writeLine(e Event) {
	if l.file == nil {
		return
	}
	line, err := json.Marshal(e)
	if err != nil {
		l.fileErr = errors.Join(l.fileErr, err)
		return
	}
	line = append(line, '\n')
	if l.size > 0 && l.size+int64(len(line)) > l.maxBytes {
		if err := l.rotate(); err != nil {
			l.fileErr = errors.Join(l.fileErr, err)
			return
		}
	}
	n, err := l.file.Write(line)
	l.size += int64(n)
	if err != nil {
		l.fileErr = errors.Join(l.fileErr, err)
	}
}

func (l *Log) rotate() error {
	err := l.file.Close()
	l.file = nil
	if rerr := os.Rename(l.path, l.path+".1"); rerr != nil {
		err = errors.Join(err, rerr)
	}
	if oerr := l.openFile(); oerr != nil {
		err = errors.Join(err, oerr)
	}
	return err
}

// Recent returns up to the last n events, oldest first.
func (l *Log) Recent(n int) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n > l.ringCap {
		n = l.ringCap
	}
	if n > len(l.ring) {
		n = len(l.ring)
	}
	if n <= 0 {
		return nil
	}
	return append([]Event(nil), l.ring[len(l.ring)-n:]...)
}

// SwapCount returns how many swap events since Open name secret in
// namespace ns. It counts every event, not only those still in the ring.
func (l *Log) SwapCount(ns, secret string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.swaps[ns][secret]
}

// Subscribe returns a channel (buffer 256) of new events and a cancel
// function that closes it. A subscriber that falls behind loses events.
func (l *Log) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, subscriberSize)
	l.mu.Lock()
	id := l.nextSub
	l.nextSub++
	l.subs[id] = ch
	l.mu.Unlock()
	return ch, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if _, ok := l.subs[id]; ok {
			delete(l.subs, id)
			close(ch)
		}
	}
}

// Close closes the file and returns any file error met since Open.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	err := l.fileErr
	if l.file != nil {
		err = errors.Join(err, l.file.Close())
		l.file = nil
	}
	return err
}
