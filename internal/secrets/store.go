// Package secrets runs the owner's fetch commands and keeps the values in
// memory only. A value never reaches an event, an error or a status field.
package secrets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/abrose/sbx-broker/internal/config"
	"github.com/abrose/sbx-broker/internal/eventlog"
	"github.com/abrose/sbx-broker/internal/placeholder"
)

// State is where a secret stands.
type State string

const (
	StatePending   State = "pending"
	StateOK        State = "ok"
	StateFailed    State = "failed"
	StateUndefined State = "undefined"
)

const (
	minValueLen = 8
	maxStdout   = 64 << 10
	maxStderr   = 4 << 10
	maxErrLen   = 200
	waitDelay   = time.Second
)

// Status describes one secret. It holds no value, only a fingerprint.
type Status struct {
	Name, Placeholder string
	State             State
	FetchedAt         time.Time
	ExitCode          int
	Err               string // one line, never stdout
	Length            int
	Fingerprint       string // the first 8 hex characters of the SHA-256 of the value
}

type entry struct {
	def   config.Secret
	run   sync.Mutex // one fetch of this secret at a time
	state Status     // guarded by Store.mu
	value []byte     // guarded by Store.mu; nil unless state is ok
}

// Store is safe for concurrent use.
type Store struct {
	ns      string
	timeout time.Duration
	log     *eventlog.Log
	retire  func(name string, v []byte)
	env     []string // of the fetch commands; nil: the broker's own
	mu      sync.RWMutex
	entries map[string]*entry
	closed  bool // guarded by mu; set by Wipe
}

// NewStore makes a store with every secret pending. When a fetch drops an ok
// value (it fails) or replaces it with another, retire, if not nil, gets a
// copy of the old value first, while the store still serves it: an upstream
// may have stored the value, so the answer scrub must still know it. retire
// owns the copy and runs with no lock of the store held. env is the
// environment of the fetch commands; nil means the broker's own.
func NewStore(ns string, defs []config.Secret, timeout time.Duration, log *eventlog.Log, retire func(name string, v []byte), env []string) *Store {
	s := &Store{ns: ns, timeout: timeout, log: log, retire: retire, env: env, entries: map[string]*entry{}}
	for _, d := range defs {
		s.entries[d.Name] = &entry{def: d, state: Status{
			Name: d.Name, Placeholder: placeholder.Format(ns, d.Name), State: StatePending,
		}}
	}
	return s
}

// FetchAll runs every fetch at the same time and returns when all are done,
// so one slow command does not hold back the other secrets.
func (s *Store) FetchAll(ctx context.Context) {
	var wg sync.WaitGroup
	for name := range s.entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Fetch(ctx, name) // the outcome is in the status and the log
		}()
	}
	wg.Wait()
}

// Fetch runs the command of one secret. A second Fetch of the same secret
// waits for the running one, then runs again, so the later result wins.
func (s *Store) Fetch(ctx context.Context, name string) error {
	e, ok := s.entries[name]
	if !ok {
		return fmt.Errorf("unknown secret %q", name)
	}
	e.run.Lock()
	defer e.run.Unlock()

	value, exit, err := s.runCommand(ctx, e.def.Command)
	s.record(e, value, exit, err)
	return err
}

// record runs under e.run, so only Wipe can change e.value meanwhile.
func (s *Store) record(e *entry, value []byte, exit int, err error) {
	s.retireOld(e, value, err)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		wipe(value)
		return
	}
	old := e.value
	st := e.state
	st.FetchedAt, st.ExitCode = time.Now(), exit
	if err != nil {
		// A failed re-fetch of an ok secret turns it failed and drops the
		// old value: no stale value is ever served.
		st.State, st.Err, st.Length, st.Fingerprint = StateFailed, err.Error(), 0, ""
		e.value = nil
	} else {
		sum := sha256.Sum256(value)
		st.State, st.Err, st.Length = StateOK, "", len(value)
		st.Fingerprint = hex.EncodeToString(sum[:])[:8]
		e.value = value
		lock(value)
	}
	e.state = st
	s.mu.Unlock()
	wipe(old)

	ev := eventlog.Event{Kind: eventlog.KindFetch, Namespace: s.ns, Secrets: []string{e.def.Name}, Message: string(st.State)}
	if err != nil {
		ev.Message = "failed: " + logReason(err)
	}
	s.log.Add(ev)
}

// retireOld hands a copy of the ok value of e to retire when this fetch
// result drops it or replaces it with another value.
func (s *Store) retireOld(e *entry, value []byte, err error) {
	if s.retire == nil {
		return
	}
	var old []byte
	s.mu.RLock()
	if !s.closed && e.value != nil && (err != nil || !bytes.Equal(e.value, value)) {
		old = append([]byte(nil), e.value...)
	}
	s.mu.RUnlock()
	if old != nil {
		s.retire(e.def.Name, old)
	}
}

// fetchError is a failed fetch. Its text (the status Err) may be the first
// stderr line of the command, which a fetch tool may fill with a value while
// it fails; the redactor does not know such a value. So the log gets only
// reason, a fixed text or the exit code.
type fetchError struct{ text, reason string }

func (e *fetchError) Error() string { return e.text }

// fail returns a fetchError whose text is also safe for the log.
func fail(text string) error { return &fetchError{text: text, reason: text} }

func logReason(err error) string {
	var fe *fetchError
	if errors.As(err, &fe) {
		return fe.reason
	}
	return "error"
}

// runCommand returns the value, the exit code and an error whose text is one
// line and never holds stdout.
func (s *Store) runCommand(ctx context.Context, argv []string) ([]byte, int, error) {
	if len(argv) == 0 {
		return nil, -1, fail("empty command")
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	var stdout, stderr capBuffer
	stdout.max, stderr.max = maxStdout+1, maxStderr // +1 for the trailing newline
	// The buffers hold the value (stderr may too): zero them on every path.
	// Grown to the cap at once, a buffer never moves, so no earlier copy of
	// its bytes is left behind unwiped.
	stdout.buf.Grow(stdout.max)
	stderr.buf.Grow(stderr.max)
	defer func() {
		clear(stdout.buf.Bytes())
		clear(stderr.buf.Bytes())
	}()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = nil // /dev/null, never the terminal
	cmd.Env = s.env
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = waitDelay

	err := cmd.Run()
	exit := -1
	if cmd.ProcessState != nil {
		exit = cmd.ProcessState.ExitCode()
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return nil, exit, fail(fmt.Sprintf("timeout after %s", s.timeout))
	case ctx.Err() != nil:
		return nil, exit, fail("cancelled")
	case err != nil:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			reason := fmt.Sprintf("exit %d", exit)
			if line := firstLine(stderr.buf.Bytes()); line != "" {
				return nil, exit, &fetchError{text: line, reason: reason}
			}
			return nil, exit, &fetchError{text: fmt.Sprintf("exit status %d", exit), reason: reason}
		}
		return nil, exit, &fetchError{text: oneLine(err.Error()), reason: "command did not start"}
	}
	value := bytes.TrimSuffix(stdout.buf.Bytes(), []byte("\n"))
	if stdout.over || len(value) > maxStdout {
		return nil, exit, fail("value longer than 64 KiB")
	}
	if len(value) < minValueLen {
		return nil, exit, fail(fmt.Sprintf("value shorter than %d bytes", minValueLen))
	}
	return append([]byte(nil), value...), exit, nil
}

// Wipe zeroes and unpins every value and closes the store: every secret
// turns pending, and a fetch that ends later wipes its value at once and
// logs nothing. Call it when the store is dropped, after its fetches end.
func (s *Store) Wipe() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for _, e := range s.entries {
		wipe(e.value)
		e.value = nil
		e.state.State, e.state.Length, e.state.Fingerprint = StatePending, 0, ""
	}
}

// Lookup returns a copy of the value, or nil when the state is not ok.
// The state is undefined for an unknown name.
func (s *Store) Lookup(name string) ([]byte, State) {
	e, ok := s.entries[name]
	if !ok {
		return nil, StateUndefined
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if e.state.State != StateOK {
		return nil, e.state.State
	}
	return append([]byte(nil), e.value...), StateOK
}

// Values returns copies of the values of the ok secrets, by name.
func (s *Store) Values() map[string][]byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string][]byte{}
	for name, e := range s.entries {
		if e.state.State == StateOK {
			out[name] = append([]byte(nil), e.value...)
		}
	}
	return out
}

// Statuses returns the status of every secret, sorted by name.
func (s *Store) Statuses() []Status {
	s.mu.RLock()
	out := make([]Status, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e.state)
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// capBuffer keeps the first max bytes and drops the rest, but still reports
// every byte as written so the child never blocks on a full pipe.
type capBuffer struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (c *capBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := c.max - c.buf.Len(); n > room {
		c.over = true
		p = p[:room]
	}
	c.buf.Write(p)
	return n, nil
}

func firstLine(b []byte) string {
	line, _, _ := strings.Cut(string(b), "\n")
	return oneLine(line)
}

// oneLine keeps the first line of s, at most maxErrLen characters.
func oneLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > maxErrLen {
		s = string(r[:maxErrLen])
	}
	return s
}

// lock pins the value in RAM, best effort.
func lock(b []byte) {
	if len(b) > 0 {
		_ = syscall.Mlock(b)
	}
}

func wipe(b []byte) {
	if len(b) == 0 {
		return
	}
	for i := range b {
		b[i] = 0
	}
	_ = syscall.Munlock(b)
}
