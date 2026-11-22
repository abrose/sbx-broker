package admin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/abrose/sbx-broker/internal/eventlog"
	"github.com/abrose/sbx-broker/internal/secrets"
)

type fakeBackend struct {
	mu        sync.Mutex
	refreshed []string // "ns/secret"
	reloads   int
}

func (f *fakeBackend) Status(ns string) ([]NamespaceStatus, error) {
	if ns != "" && ns != "t1" {
		return nil, fmt.Errorf("unknown namespace %s", ns)
	}
	return []NamespaceStatus{{
		Name:      "t1",
		Listeners: []string{"127.0.0.1:29691"},
		Secrets:   []secrets.Status{{Name: "api-key", Placeholder: "sbxb_t1_api_key", State: secrets.StateOK, Length: 14}},
	}}, nil
}

func (f *fakeBackend) Refresh(_ context.Context, ns, secret string) ([]NamespaceStatus, error) {
	f.mu.Lock()
	f.refreshed = append(f.refreshed, ns+"/"+secret)
	f.mu.Unlock()
	return f.Status(ns)
}

func (f *fakeBackend) Reload(context.Context) error {
	f.mu.Lock()
	f.reloads++
	f.mu.Unlock()
	return nil
}

// serve starts an admin server in a short state dir (ruling R5).
func serve(t *testing.T) (string, *fakeBackend, *eventlog.Log, *Server) {
	t.Helper()
	state, err := os.MkdirTemp("/tmp", "sbxb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	log, err := eventlog.Open(filepath.Join(state, "broker.log"), 1000, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := TakeLock(state)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := lock.Listen()
	if err != nil {
		t.Fatal(err)
	}
	b := &fakeBackend{}
	s := Serve(ln, b, log)
	t.Cleanup(func() {
		_ = s.Close()
		_ = log.Close()
		_ = lock.Release()
	})
	return state, b, log, s
}

func do(t *testing.T, state string, req Request) ([]Response, error) {
	t.Helper()
	c, err := Dial(state)
	if err != nil {
		t.Fatal(err)
	}
	var out []Response
	err = c.Do(req, func(r Response) error {
		out = append(out, r)
		return nil
	})
	return out, err
}

func TestStatus(t *testing.T) {
	state, _, _, _ := serve(t)
	resps, err := do(t, state, Request{Op: "status"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resps) != 1 || !resps[0].OK || len(resps[0].Namespaces) != 1 || resps[0].Namespaces[0].Secrets[0].Name != "api-key" {
		t.Fatalf("got %+v", resps)
	}
	if _, err := do(t, state, Request{Op: "status", Namespace: "t9"}); err == nil || err.Error() != "unknown namespace t9" {
		t.Fatalf("got %v", err)
	}
}

func TestRefreshOneSecret(t *testing.T) {
	state, b, _, _ := serve(t)
	resps, err := do(t, state, Request{Op: "refresh", Namespace: "t1", Secret: "api-key"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resps) != 1 || len(resps[0].Namespaces) != 1 {
		t.Fatalf("got %+v", resps)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.refreshed) != 1 || b.refreshed[0] != "t1/api-key" {
		t.Fatalf("refreshed %v", b.refreshed)
	}
}

func TestReload(t *testing.T) {
	state, b, _, _ := serve(t)
	if _, err := do(t, state, Request{Op: "reload"}); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.reloads != 1 {
		t.Fatalf("reloads %d", b.reloads)
	}
}

func TestUnknownOp(t *testing.T) {
	state, _, _, _ := serve(t)
	if _, err := do(t, state, Request{Op: "nope"}); err == nil || err.Error() != "unknown op nope" {
		t.Fatalf("got %v", err)
	}
}

func TestLogLast100(t *testing.T) {
	state, _, log, _ := serve(t)
	for i := range 150 {
		ns := "t1"
		if i%2 == 1 {
			ns = "t2"
		}
		log.Add(eventlog.Event{Kind: eventlog.KindInfo, Namespace: ns, Message: fmt.Sprint(i)})
	}
	resps, err := do(t, state, Request{Op: "log"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resps) != 100 || resps[0].Event.Message != "50" || resps[99].Event.Message != "149" {
		t.Fatalf("got %d events", len(resps))
	}
	resps, err = do(t, state, Request{Op: "log", Namespace: "t2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resps) != 75 || resps[0].Event.Message != "1" {
		t.Fatalf("got %d events", len(resps))
	}
}

var errStop = errors.New("stop")

func TestLogFollow(t *testing.T) {
	state, _, log, _ := serve(t)
	log.Add(eventlog.Event{Kind: eventlog.KindInfo, Namespace: "t1", Message: "old"})
	log.Add(eventlog.Event{Kind: eventlog.KindInfo, Namespace: "t2", Message: "other"})
	c, err := Dial(state)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	done := make(chan error, 1)
	go func() {
		done <- c.Do(Request{Op: "log", Namespace: "t1", Follow: true}, func(r Response) error {
			got = append(got, r.Event.Message)
			if r.Event.Message == "new" {
				return errStop
			}
			return nil
		})
	}()
	time.Sleep(100 * time.Millisecond)
	log.Add(eventlog.Event{Kind: eventlog.KindInfo, Namespace: "t2", Message: "skip"})
	log.Add(eventlog.Event{Kind: eventlog.KindInfo, Namespace: "t1", Message: "new"})
	select {
	case err := <-done:
		if !errors.Is(err, errStop) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no new event")
	}
	if len(got) != 2 || got[0] != "old" || got[1] != "new" {
		t.Fatalf("got %v", got)
	}
}

// Close ends a follow that waits for events.
func TestCloseEndsFollow(t *testing.T) {
	state, _, _, s := serve(t)
	c, err := Dial(state)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Do(Request{Op: "log", Follow: true}, func(Response) error { return nil }) }()
	time.Sleep(100 * time.Millisecond)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("follow did not end")
	}
}

func TestTakeLockRefusesRunningBroker(t *testing.T) {
	state, _, _, _ := serve(t)
	if _, err := TakeLock(state); !errors.Is(err, ErrRunning) {
		t.Fatalf("got %v", err)
	}
}

// Closing the server (and its listener) keeps the lock: the broker still
// shuts down. Only Release frees it.
func TestServerCloseKeepsLock(t *testing.T) {
	state, _, _, s := serve(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if l, err := TakeLock(state); !errors.Is(err, ErrRunning) {
		if l != nil {
			_ = l.Release()
		}
		t.Fatalf("got %v", err)
	}
}

func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sbxb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// The lock decides, not a dial: a broker that holds admin.lock runs even
// when no socket answers (such as a full backlog).
func TestListenRefusesWhileLockHeld(t *testing.T) {
	state := shortDir(t)
	f, err := os.OpenFile(filepath.Join(state, LockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if l, err := TakeLock(state); !errors.Is(err, ErrRunning) {
		if l != nil {
			_ = l.Release()
		}
		t.Fatalf("got %v", err)
	}
}

// With the lock held, any socket file is stale, even one that accepts: it
// is replaced without a probe.
func TestListenReplacesSocketWithoutLock(t *testing.T) {
	state := shortDir(t)
	old, err := net.Listen("unix", SocketPath(state))
	if err != nil {
		t.Fatal(err)
	}
	old.(*net.UnixListener).SetUnlinkOnClose(false)
	defer old.Close()
	lock, err := TakeLock(state)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	ln, err := lock.Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if c, err := ln.Accept(); err == nil {
			_ = c.Close()
		}
	}()
	c, err := net.Dial("unix", SocketPath(state))
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the new listener did not get the connection")
	}
}

func TestReleaseFreesLock(t *testing.T) {
	state := shortDir(t)
	lock, err := TakeLock(state)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(state, LockName))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("lock mode %04o", fi.Mode().Perm())
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
	again, err := TakeLock(state)
	if err != nil {
		t.Fatalf("after Release: %v", err)
	}
	_ = again.Release()
}

func TestDialNoBroker(t *testing.T) {
	state, err := os.MkdirTemp("/tmp", "sbxb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	if _, err := Dial(state); !errors.Is(err, ErrNoBroker) {
		t.Fatalf("got %v", err)
	}
}
