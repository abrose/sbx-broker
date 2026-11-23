package secrets

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abrose/sbx-broker/internal/config"
	"github.com/abrose/sbx-broker/internal/eventlog"
)

func newTestStore(t *testing.T, timeout time.Duration, defs ...config.Secret) (*Store, string) {
	t.Helper()
	return newRetiringStore(t, timeout, nil, defs...)
}

func newRetiringStore(t *testing.T, timeout time.Duration, retire func(string, []byte), defs ...config.Secret) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "broker.log")
	log, err := eventlog.Open(path, 100, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return NewStore("t1", defs, timeout, log, retire, nil), path
}

// A fetch that drops or changes an ok value hands a copy of the old value to
// the retire func before the store lets it go (finding C1). The same value
// again, or a failure with no value, retires nothing.
func TestFetchRetiresDroppedValue(t *testing.T) {
	valueFile := filepath.Join(t.TempDir(), "value")
	write := func(v string) {
		if err := os.WriteFile(valueFile, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	s, _ := newRetiringStore(t, 5*time.Second, func(name string, v []byte) {
		got = append(got, name+"="+string(v))
	}, def("a", "cat", valueFile))
	fetch := func() { _ = s.Fetch(context.Background(), "a") }

	write("value-1111")
	fetch()
	fetch() // the same value
	write("value-2222")
	fetch() // a new value
	if err := os.Remove(valueFile); err != nil {
		t.Fatal(err)
	}
	fetch() // fails
	fetch() // fails again: no value to retire
	want := []string{"a=value-1111", "a=value-2222"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("retired %q, want %q", got, want)
	}
}

func def(name string, cmd ...string) config.Secret {
	return config.Secret{Name: name, Command: cmd}
}

func status(t *testing.T, s *Store, name string) Status {
	t.Helper()
	for _, st := range s.Statuses() {
		if st.Name == name {
			return st
		}
	}
	t.Fatalf("no status for %s", name)
	return Status{}
}

func TestFetchOK(t *testing.T) {
	s, _ := newTestStore(t, 5*time.Second, def("api-key", "printf", "%s\n", "value-1234"))
	if err := s.Fetch(context.Background(), "api-key"); err != nil {
		t.Fatal(err)
	}
	v, st := s.Lookup("api-key")
	if st != StateOK || string(v) != "value-1234" {
		t.Fatalf("got %q %s", v, st)
	}
	got := status(t, s, "api-key")
	if got.Length != 10 || len(got.Fingerprint) != 8 || got.Placeholder != "sbxb_t1_api_key" || got.FetchedAt.IsZero() {
		t.Fatalf("status %+v", got)
	}
}

func TestFetchStripsOneNewline(t *testing.T) {
	s, _ := newTestStore(t, 5*time.Second, def("a", "printf", "value-1234\n\n"))
	_ = s.Fetch(context.Background(), "a")
	if v, _ := s.Lookup("a"); string(v) != "value-1234\n" {
		t.Fatalf("got %q", v)
	}
}

func TestFetchFails(t *testing.T) {
	s, _ := newTestStore(t, 5*time.Second, def("a", "sh", "-c", "echo oops >&2; exit 3"))
	if err := s.Fetch(context.Background(), "a"); err == nil {
		t.Fatal("want error")
	}
	st := status(t, s, "a")
	if st.State != StateFailed || st.ExitCode != 3 || st.Err != "oops" {
		t.Fatalf("status %+v", st)
	}
	if v, state := s.Lookup("a"); v != nil || state != StateFailed {
		t.Fatalf("got %q %s", v, state)
	}
}

func TestFetchTimeout(t *testing.T) {
	s, _ := newTestStore(t, 200*time.Millisecond, def("a", "sh", "-c", "sleep 30"))
	start := time.Now()
	_ = s.Fetch(context.Background(), "a")
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %s", d)
	}
	st := status(t, s, "a")
	if st.State != StateFailed || !strings.Contains(st.Err, "timeout") {
		t.Fatalf("status %+v", st)
	}
}

func TestFetchRefusesShortValue(t *testing.T) {
	s, _ := newTestStore(t, 5*time.Second, def("a", "printf", "short"))
	_ = s.Fetch(context.Background(), "a")
	st := status(t, s, "a")
	if st.State != StateFailed || !strings.Contains(st.Err, "shorter than 8") {
		t.Fatalf("status %+v", st)
	}
	if strings.Contains(st.Err, "short\n") || st.Err == "short" {
		t.Fatal("Err holds stdout")
	}
}

func TestFetchNoStdin(t *testing.T) {
	s, _ := newTestStore(t, 5*time.Second, def("a", "sh", "-c", "read x; printf %s value-$x-1234"))
	_ = s.Fetch(context.Background(), "a")
	if v, st := s.Lookup("a"); st != StateOK || string(v) != "value--1234" {
		t.Fatalf("got %q %s", v, st)
	}
}

func TestStdoutNeverLogged(t *testing.T) {
	s, path := newTestStore(t, 5*time.Second,
		def("a", "sh", "-c", "echo leak-value-99; echo oops >&2; exit 3"),
		def("b", "printf", "leak-value-98"))
	_ = s.Fetch(context.Background(), "a")
	_ = s.Fetch(context.Background(), "b")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "leak-value-9") {
		t.Fatalf("log holds stdout: %s", data)
	}
	if !strings.Contains(string(data), `"kind":"fetch"`) || !strings.Contains(string(data), "failed: exit 3") {
		t.Fatalf("log lacks fetch event: %s", data)
	}
	if st := status(t, s, "a"); strings.Contains(st.Err, "leak") {
		t.Fatal("Err holds stdout")
	}
}

// A fetch tool may print a value on stderr while it fails. The first stderr
// line goes to the status (the owner's terminal), never to the log.
func TestStderrNeverLogged(t *testing.T) {
	s, path := newTestStore(t, 200*time.Millisecond,
		def("a", "sh", "-c", "echo stderr-value-77 >&2; exit 3"),
		def("b", "sh", "-c", "echo stderr-value-76 >&2; sleep 30"),
		def("c", "printf", "short"),
		def("d", "no-such-command-sbxb"))
	s.FetchAll(context.Background())
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "stderr-value-7") {
		t.Fatal("log holds a stderr line")
	}
	for _, want := range []string{"failed: exit 3", "failed: timeout after 200ms", "failed: value shorter than 8 bytes", "failed: command did not start"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("log lacks %q: %s", want, data)
		}
	}
	if st := status(t, s, "a"); st.Err != "stderr-value-77" {
		t.Fatal("status lacks the stderr line")
	}
}

func TestLookupUndefined(t *testing.T) {
	s, _ := newTestStore(t, time.Second)
	if v, st := s.Lookup("nope"); v != nil || st != StateUndefined {
		t.Fatalf("got %q %s", v, st)
	}
	if err := s.Fetch(context.Background(), "nope"); err == nil {
		t.Fatal("want error for unknown name")
	}
}

func TestPendingBeforeFetch(t *testing.T) {
	s, _ := newTestStore(t, time.Second, def("a", "printf", "value-1234"))
	if _, st := s.Lookup("a"); st != StatePending {
		t.Fatalf("got %s", st)
	}
}

func TestFetchAllRunsInParallel(t *testing.T) {
	s, _ := newTestStore(t, 10*time.Second,
		def("slow", "sh", "-c", "sleep 3; printf value-slow-1"),
		def("quick", "printf", "value-quick-1"))
	done := make(chan struct{})
	go func() { s.FetchAll(context.Background()); close(done) }()
	deadline := time.Now().Add(time.Second)
	for {
		if _, st := s.Lookup("quick"); st == StateOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("quick not ok within 1s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("FetchAll returned before the slow fetch ended")
	default:
	}
	if _, st := s.Lookup("slow"); st != StatePending {
		t.Fatalf("slow is %s", st)
	}
	<-done
	if _, st := s.Lookup("slow"); st != StateOK {
		t.Fatalf("slow is %s after FetchAll", st)
	}
}

func TestFailedRefetchDropsOldValue(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "m")
	s, _ := newTestStore(t, 5*time.Second,
		def("a", "sh", "-c", "if [ -e "+marker+" ]; then exit 1; fi; printf value-1234"))
	_ = s.Fetch(context.Background(), "a")
	if _, st := s.Lookup("a"); st != StateOK {
		t.Fatal("first fetch failed")
	}
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = s.Fetch(context.Background(), "a")
	if v, st := s.Lookup("a"); st != StateFailed || v != nil {
		t.Fatalf("got %q %s", v, st)
	}
	if _, ok := s.Values()["a"]; ok {
		t.Fatal("Values still holds a failed secret")
	}
}

func TestValuesReturnsCopies(t *testing.T) {
	s, _ := newTestStore(t, 5*time.Second, def("a", "printf", "value-1234"))
	_ = s.Fetch(context.Background(), "a")
	s.Values()["a"][0] = 'X'
	lv, _ := s.Lookup("a")
	lv[1] = 'Y'
	if v, _ := s.Lookup("a"); string(v) != "value-1234" {
		t.Fatalf("stored value changed: %q", v)
	}
}

func TestFetchRefusesLongValue(t *testing.T) {
	s, _ := newTestStore(t, 5*time.Second, def("a", "sh", "-c", "head -c 70000 /dev/zero | tr '\\0' x"))
	_ = s.Fetch(context.Background(), "a")
	st := status(t, s, "a")
	if st.State != StateFailed || st.Err != "value longer than 64 KiB" {
		t.Fatalf("status %+v", st)
	}
}

func TestErrIsOneShortLine(t *testing.T) {
	s, _ := newTestStore(t, 5*time.Second,
		def("a", "sh", "-c", "head -c 500 /dev/zero | tr '\\0' e >&2; echo >&2; echo second >&2; exit 1"))
	_ = s.Fetch(context.Background(), "a")
	st := status(t, s, "a")
	if len(st.Err) != 200 || strings.Contains(st.Err, "\n") {
		t.Fatalf("Err %q", st.Err)
	}
}

func TestStatusesSortedAndConcurrent(t *testing.T) {
	s, _ := newTestStore(t, 5*time.Second,
		def("b", "printf", "value-bbbb"), def("a", "printf", "value-aaaa"), def("c", "printf", "value-cccc"))
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
				s.Statuses()
				s.Values()
				s.Lookup("a")
			}
		}
	}()
	s.FetchAll(context.Background())
	close(done)
	sts := s.Statuses()
	if len(sts) != 3 || sts[0].Name != "a" || sts[1].Name != "b" || sts[2].Name != "c" {
		t.Fatalf("got %+v", sts)
	}
}

func TestSecondFetchWhileRunning(t *testing.T) {
	s, _ := newTestStore(t, 5*time.Second, def("a", "sh", "-c", "sleep 0.3; printf value-1234"))
	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- s.Fetch(context.Background(), "a") }()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if _, st := s.Lookup("a"); st != StateOK {
		t.Fatal(st)
	}
}

// Wipe zeroes every value and closes the store.
func TestWipeZeroesValues(t *testing.T) {
	s, _ := newTestStore(t, 5*time.Second, def("a", "printf", "%s", "value-aaaa-1"), def("b", "printf", "%s", "value-bbbb-2"))
	s.FetchAll(context.Background())
	held := [][]byte{s.entries["a"].value, s.entries["b"].value}
	s.Wipe()
	for i, v := range held {
		if len(v) == 0 || strings.Trim(string(v), "\x00") != "" {
			t.Errorf("value %d is not zeroed", i)
		}
	}
	for _, name := range []string{"a", "b"} {
		if v, st := s.Lookup(name); st == StateOK || v != nil {
			t.Errorf("%s: state %s after Wipe", name, st)
		}
	}
	if n := len(s.Values()); n != 0 {
		t.Errorf("%d values after Wipe", n)
	}
}

// A fetch that ends after Wipe leaves no value.
func TestFetchAfterWipeKeepsNoValue(t *testing.T) {
	s, _ := newTestStore(t, 5*time.Second, def("slow", "sh", "-c", "sleep 0.3; printf %s value-late-3"))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Fetch(context.Background(), "slow")
	}()
	time.Sleep(50 * time.Millisecond)
	s.Wipe()
	<-done
	if v, st := s.Lookup("slow"); st == StateOK || v != nil {
		t.Fatalf("state %s after a late fetch", st)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.entries["slow"].value != nil {
		t.Fatal("the late value is kept")
	}
}
