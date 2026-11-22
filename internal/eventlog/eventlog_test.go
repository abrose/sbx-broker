package eventlog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func openLog(t *testing.T, ring int, maxBytes int64) (*Log, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "broker.log")
	l, err := Open(path, ring, maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, path
}

func TestPrintable(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain text, ok: /v1?a=b", "plain text, ok: /v1?a=b"},
		{"\x1b[2J", `\x1b[2J`},
		{"a\nb\r\tc", `a\x0ab\x0d\x09c`},
		{"del\x7f", `del\x7f`},
		{"c1 \u009b31m", `c1 \x9b31m`},
		{"bad \xff\xfe utf8", `bad \xff\xfe utf8`},
		{`back\slash`, `back\\slash`},
		{"umlaut äöü and �", "umlaut äöü and �"},
	} {
		if got := Printable(tc.in); got != tc.want {
			t.Errorf("Printable(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPrintableField(t *testing.T) {
	if got, want := PrintableField("/a b\x1b[2J"), `/a%20b\x1b[2J`; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRingKeepsLastN(t *testing.T) {
	l, _ := openLog(t, 3, 1<<20)
	for i := 1; i <= 5; i++ {
		l.Add(Event{Kind: KindInfo, Status: i})
	}
	got := l.Recent(10)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	for i, want := range []int{3, 4, 5} {
		if got[i].Status != want {
			t.Errorf("got[%d].Status = %d, want %d", i, got[i].Status, want)
		}
	}
	if last := l.Recent(1); len(last) != 1 || last[0].Status != 5 {
		t.Errorf("Recent(1) = %+v", last)
	}
}

func TestFileIsJSONLines(t *testing.T) {
	l, path := openLog(t, 10, 1<<20)
	l.Add(Event{Kind: KindSwap, Namespace: "t1", Secrets: []string{"api-key"}})
	l.Add(Event{Kind: KindRefuse, Status: 503})

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %o, want 600", mode)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var events []Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		events = append(events, e)
	}
	if len(events) != 2 {
		t.Fatalf("lines = %d, want 2", len(events))
	}
	if events[0].Kind != KindSwap || events[0].Namespace != "t1" || events[0].Secrets[0] != "api-key" {
		t.Errorf("first = %+v", events[0])
	}
	if events[0].Time.IsZero() {
		t.Error("Time was not set")
	}
}

func TestRotates(t *testing.T) {
	l, path := openLog(t, 10, 500)
	for i := 0; i < 20; i++ {
		l.Add(Event{Kind: KindInfo, Message: strings.Repeat("x", 40)})
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("broker.log.1 missing: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() >= 500 {
		t.Errorf("broker.log size = %d, want < 500", info.Size())
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("new file mode = %o, want 600", info.Mode().Perm())
	}
	old, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if old.Mode().Perm() != 0o600 {
		t.Errorf(".1 mode = %o, want 600", old.Mode().Perm())
	}
}

func TestRedactor(t *testing.T) {
	l, path := openLog(t, 10, 1<<20)
	l.SetRedactor(func(s string) string {
		return strings.ReplaceAll(s, "s3cr3t-value", "[redacted]")
	})
	l.Add(Event{Kind: KindError, Message: "x s3cr3t-value", Path: "/a/s3cr3t-value"})

	got := l.Recent(1)[0]
	if got.Message != "x [redacted]" || got.Path != "/a/[redacted]" {
		t.Errorf("ring = %+v", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "s3cr3t-value") || !strings.Contains(string(data), `"x [redacted]"`) {
		t.Errorf("file = %s", data)
	}
}

func TestRedactorAppliesToSubscribers(t *testing.T) {
	l, _ := openLog(t, 10, 1<<20)
	l.SetRedactor(func(s string) string { return strings.ReplaceAll(s, "s3cr3t-value", "[redacted]") })
	ch, cancel := l.Subscribe()
	defer cancel()
	l.Add(Event{Kind: KindError, Message: "s3cr3t-value"})
	select {
	case e := <-ch:
		if e.Message != "[redacted]" {
			t.Errorf("subscriber saw %q", e.Message)
		}
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
}

func TestSubscribeAndCancel(t *testing.T) {
	l, _ := openLog(t, 10, 1<<20)
	ch, cancel := l.Subscribe()
	l.Add(Event{Kind: KindInfo, Message: "one"})
	if e := <-ch; e.Message != "one" {
		t.Errorf("got %+v", e)
	}
	cancel()
	cancel() // idempotent
	if _, ok := <-ch; ok {
		t.Error("channel should be closed after cancel")
	}
	l.Add(Event{Kind: KindInfo, Message: "two"}) // must not panic
}

func TestSlowSubscriberDoesNotBlock(t *testing.T) {
	l, _ := openLog(t, 10, 1<<20)
	_, cancel := l.Subscribe() // never read
	defer cancel()
	start := time.Now()
	for i := 0; i < 1000; i++ {
		l.Add(Event{Kind: KindInfo})
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("1000 Adds took %v", d)
	}
}

func TestConcurrentAdd(t *testing.T) {
	l, path := openLog(t, 100, 1<<20)
	ch, cancel := l.Subscribe()
	defer cancel()
	go func() {
		for range ch {
		}
	}()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				l.Add(Event{Kind: KindInfo})
				_ = l.Recent(5)
			}
		}()
	}
	wg.Wait()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "\n"); n != 800 {
		t.Errorf("lines = %d, want 800", n)
	}
}

func TestOpenTightensExistingFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broker.log")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil { // defeat umask
		t.Fatal(err)
	}
	l, err := Open(path, 10, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %o, want 600", mode)
	}
}

func TestOpenAppendsToExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broker.log")
	l, err := Open(path, 10, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	l.Add(Event{Kind: KindInfo})
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l, err = Open(path, 10, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	l.Add(Event{Kind: KindInfo})
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if n := strings.Count(string(data), "\n"); n != 2 {
		t.Errorf("lines = %d, want 2", n)
	}
}
