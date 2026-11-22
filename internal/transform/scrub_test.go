package transform

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/abrose/sbx-broker/internal/config"
)

var (
	stdRun = regexp.MustCompile(`[A-Za-z0-9+/]+`)
	urlRun = regexp.MustCompile(`[A-Za-z0-9_-]+`)
)

// decodings returns every Base64 decoding of every run of Base64 characters
// in s, in both alphabets and at all four character offsets, so a value at
// any position in a longer Base64 text shows up in one of them.
func decodings(s string) [][]byte {
	var out [][]byte
	for _, alpha := range []struct {
		run *regexp.Regexp
		enc *base64.Encoding
	}{{stdRun, base64.RawStdEncoding}, {urlRun, base64.RawURLEncoding}} {
		for _, run := range alpha.run.FindAllString(s, -1) {
			for off := 0; off < 4 && off < len(run); off++ {
				sub := run[off:]
				if len(sub)%4 == 1 {
					sub = sub[:len(sub)-1]
				}
				if d, err := alpha.enc.DecodeString(sub); err == nil {
					out = append(out, d)
				}
			}
		}
	}
	return out
}

// leaks returns the names of the test values in s, as text or in any Base64
// decoding of s.
func leaks(s string) []string {
	var found []string
	decoded := decodings(s)
	for _, name := range slices.Sorted(maps.Keys(testValues)) {
		v := []byte(testValues[name])
		hit := strings.Contains(s, testValues[name])
		for _, d := range decoded {
			hit = hit || bytes.Contains(d, v)
		}
		if hit {
			found = append(found, name)
		}
	}
	return found
}

func noLeak(t *testing.T, what, s string) {
	t.Helper()
	if found := leaks(s); len(found) > 0 {
		t.Errorf("%s: leaks %v", what, found)
	}
}

type doneRec struct {
	mu    sync.Mutex
	calls int
	names []string
}

func (d *doneRec) done(names []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	d.names = names
}

func (d *doneRec) get() (int, []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls, d.names
}

func newResp(body io.Reader) *http.Response {
	rc, ok := body.(io.ReadCloser)
	if !ok {
		rc = io.NopCloser(body)
	}
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: rc, ContentLength: -1}
}

func scrubT1(t *testing.T, resp *http.Response) *doneRec {
	t.Helper()
	tr, err := New(config.TransformSpec{Name: "swap"})
	if err != nil {
		t.Fatal(err)
	}
	rec := &doneRec{}
	if err := tr.Response(resp, "t1", t1(), rec.done); err != nil {
		t.Fatal(err)
	}
	return rec
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// stepReader gives each step in its own Read, then EOF, or blocks on block
// when it is set, or returns err when it is set.
type stepReader struct {
	steps []string
	block chan struct{}
	err   error
}

func (s *stepReader) Read(p []byte) (int, error) {
	if len(s.steps) == 0 {
		if s.block != nil {
			<-s.block
		}
		if s.err != nil {
			return 0, s.err
		}
		return 0, io.EOF
	}
	n := copy(p, s.steps[0])
	s.steps[0] = s.steps[0][n:]
	if s.steps[0] == "" {
		s.steps = s.steps[1:]
	}
	return n, nil
}

func TestScrubWholeValue(t *testing.T) {
	resp := newResp(strings.NewReader("x " + apiKey + " y"))
	scrubT1(t, resp)
	check(t, "body", readBody(t, resp), "x sbxb_t1_api_key y")
}

func TestScrubSplitAcrossReads(t *testing.T) {
	resp := newResp(&stepReader{steps: []string{"x real-api", "-key-1 y"}})
	scrubT1(t, resp)
	check(t, "body", readBody(t, resp), "x sbxb_t1_api_key y")
}

func TestScrubBase64(t *testing.T) {
	resp := newResp(strings.NewReader("v=" + b64(apiKey) + ";"))
	scrubT1(t, resp)
	check(t, "body", readBody(t, resp), "v="+b64("sbxb_t1_api_key")+";")
}

func TestScrubBase64WholeWinsOverCore(t *testing.T) {
	enc := b64(apiKey)
	for cut := 1; cut < len(enc); cut++ {
		resp := newResp(&stepReader{steps: []string{"v=" + enc[:cut], enc[cut:] + ";"}})
		scrubT1(t, resp)
		got := readBody(t, resp)
		if got != "v="+b64("sbxb_t1_api_key")+";" {
			t.Errorf("cut %d: got %s", cut, show(got))
		}
	}
}

func TestScrubBase64Embedded(t *testing.T) {
	cases := []struct{ name, text string }{
		{"basic auth echo", b64("user:" + pass)},
		{"value in the middle", b64("ab" + apiKey + "xyz")},
		{"each offset", b64(apiKey) + " " + b64("a"+apiKey) + " " + b64("ab"+odd+"c")},
	}
	for _, c := range cases {
		body := `{"echo":"` + c.text + `"}`
		if len(leaks(body)) == 0 {
			t.Fatalf("%s: the leak check does not see the value in the input", c.name)
		}
		resp := newResp(strings.NewReader(body))
		scrubT1(t, resp)
		noLeak(t, c.name, readBody(t, resp))
	}
}

func TestScrubHeaders(t *testing.T) {
	resp := newResp(strings.NewReader("ok"))
	resp.Header.Set("X-Echo", apiKey)
	resp.Header.Add("X-Multi", "a "+pass)
	resp.Header.Add("X-Multi", "b "+b64(odd))
	resp.Header.Set("Content-Length", "2")
	resp.ContentLength = 2
	scrubT1(t, resp)
	check(t, "X-Echo", resp.Header.Get("X-Echo"), "sbxb_t1_api_key")
	multi := resp.Header.Values("X-Multi")
	if len(multi) != 2 {
		t.Fatalf("X-Multi has %d values", len(multi))
	}
	check(t, "X-Multi 1", multi[0], "a sbxb_t1_pass")
	check(t, "X-Multi 2", multi[1], "b "+b64("sbxb_t1_odd"))
	if resp.Header.Get("Content-Length") != "" || resp.ContentLength != -1 {
		t.Errorf("Content-Length %q, ContentLength %d", resp.Header.Get("Content-Length"), resp.ContentLength)
	}
}

func TestScrubTrailers(t *testing.T) {
	resp := newResp(strings.NewReader("ok"))
	resp.Trailer = http.Header{"X-Sig": {"sig " + apiKey}}
	rec := scrubT1(t, resp)
	readBody(t, resp)
	check(t, "trailer", resp.Trailer.Get("X-Sig"), "sig sbxb_t1_api_key")
	if _, names := rec.get(); !slices.Equal(names, []string{"api-key"}) {
		t.Errorf("names = %v", names)
	}
}

// readOnce does one Read and fails the test when it blocks.
func readOnce(t *testing.T, r io.Reader) string {
	t.Helper()
	type result struct {
		s   string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		buf := make([]byte, 64)
		n, err := r.Read(buf)
		ch <- result{string(buf[:n]), err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("Read: %v", res.err)
		}
		return res.s
	case <-time.After(2 * time.Second):
		t.Fatal("Read blocks")
		return ""
	}
}

func TestScrubHoldsBackOnlyAPrefix(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })

	resp := newResp(&stepReader{steps: []string{"data: a\n\n"}, block: block})
	scrubT1(t, resp)
	check(t, "first Read", readOnce(t, resp.Body), "data: a\n\n")

	resp = newResp(&stepReader{steps: []string{"data: real-ap"}, block: block})
	scrubT1(t, resp)
	check(t, "Read with a prefix", readOnce(t, resp.Body), "data: ")
}

func TestScrubReportsNames(t *testing.T) {
	resp := newResp(strings.NewReader("a " + apiKey + " b " + b64(pass)))
	rec := scrubT1(t, resp)
	readBody(t, resp)
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	calls, names := rec.get()
	if calls != 1 || !slices.Equal(names, []string{"api-key", "pass"}) {
		t.Errorf("done: %d calls, names %v", calls, names)
	}
}

func TestScrubReportsHeaderNames(t *testing.T) {
	resp := newResp(strings.NewReader("a " + apiKey))
	resp.Header.Set("X-Echo", odd)
	rec := scrubT1(t, resp)
	readBody(t, resp)
	if _, names := rec.get(); !slices.Equal(names, []string{"api-key", "odd"}) {
		t.Errorf("names = %v", names)
	}
}

func TestScrubDoneOnce(t *testing.T) {
	resp := newResp(strings.NewReader("a " + apiKey))
	rec := scrubT1(t, resp)
	readBody(t, resp)
	if calls, _ := rec.get(); calls != 1 {
		t.Errorf("at EOF: %d calls, want 1", calls)
	}
	resp.Body.Close()
	resp.Body.Close()
	if calls, _ := rec.get(); calls != 1 {
		t.Errorf("after Close: %d calls, want 1", calls)
	}

	resp = newResp(strings.NewReader("a " + apiKey))
	rec = scrubT1(t, resp)
	resp.Body.Close()
	if calls, _ := rec.get(); calls != 1 {
		t.Errorf("Close before EOF: %d calls, want 1", calls)
	}
	if _, err := resp.Body.Read(make([]byte, 8)); err == nil {
		t.Error("Read after Close gives no error")
	}
	if calls, _ := rec.get(); calls != 1 {
		t.Errorf("Read after Close: %d calls, want 1", calls)
	}
}

func TestScrubDataWithEOF(t *testing.T) {
	resp := newResp(iotest.DataErrReader(strings.NewReader("x " + apiKey + " y")))
	scrubT1(t, resp)
	check(t, "body", readBody(t, resp), "x sbxb_t1_api_key y")
}

func TestScrubSmallBuffer(t *testing.T) {
	body := "x " + apiKey + " y " + b64(pass)
	want := "x sbxb_t1_api_key y " + b64("sbxb_t1_pass")
	resp := newResp(iotest.OneByteReader(strings.NewReader(body)))
	scrubT1(t, resp)
	got, err := io.ReadAll(iotest.OneByteReader(resp.Body))
	if err != nil {
		t.Fatal(err)
	}
	check(t, "body", string(got), want)
}

func TestScrubLongestMatchFirst(t *testing.T) {
	pats := map[string]string{"abc": "1", "abcdef": "2"}
	cases := []struct {
		steps []string
		want  string
		hits  []string
	}{
		{[]string{"xabcd", "ef y"}, "x2 y", []string{"2"}},
		{[]string{"xabcd", "x"}, "x1dx", []string{"1"}},
		{[]string{"xab", "c"}, "x1", []string{"1"}},
	}
	for _, c := range cases {
		r, hits := NewScrubReader(&stepReader{steps: c.steps}, pats)
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != c.want || !slices.Equal(hits(), c.hits) {
			t.Errorf("%q: got %q, hits %v", c.steps, got, hits())
		}
	}
}

func TestScrubInnerErrorDropsHeld(t *testing.T) {
	boom := errors.New("boom")
	resp := newResp(&stepReader{steps: []string{"x real-ap"}, err: boom})
	scrubT1(t, resp)
	got, err := io.ReadAll(resp.Body)
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want boom", err)
	}
	check(t, "body", string(got), "x ")
}

func TestScrubCloseWhileReading(t *testing.T) {
	pr, pw := io.Pipe()
	resp := newResp(pr)
	rec := scrubT1(t, resp)
	go func() { _, _ = pw.Write([]byte("x " + apiKey + " y")) }()
	errc := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(resp.Body)
		errc <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err == nil {
		t.Error("Read after Close gives no error")
	}
	if calls, _ := rec.get(); calls != 1 {
		t.Errorf("done: %d calls, want 1", calls)
	}
}

// cutReader gives data in pieces whose sizes come from cuts, and returns the
// last piece together with io.EOF.
type cutReader struct {
	data []byte
	cuts []byte
}

func (c *cutReader) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	size := len(c.data)
	if len(c.cuts) > 0 {
		size = int(c.cuts[0])%9 + 1
		c.cuts = c.cuts[1:]
	}
	size = min(size, len(p), len(c.data))
	n := copy(p, c.data[:size])
	c.data = c.data[n:]
	if len(c.data) == 0 {
		return n, io.EOF
	}
	return n, nil
}

func FuzzScrubSplit(f *testing.F) {
	f.Add([]byte("x "+apiKey+" y"), []byte{3, 7})
	f.Add([]byte(b64(apiKey)), []byte{5, 1, 1})
	f.Add([]byte(b64("user:"+pass)), []byte{9, 1})
	f.Add([]byte("abcabcabcab bca"), []byte{1, 2, 3})
	pats := append(responsePatterns("t1", t1().Values()),
		pattern{from: "ab", to: "X", tag: "s"}, pattern{from: "abc", to: "Y", tag: "s"},
		pattern{from: "abcab", to: "Z", tag: "s"}, pattern{from: "bca", to: "W", tag: "s"})
	m := newMatcher(pats)
	f.Fuzz(func(t *testing.T, data, cuts []byte) {
		whole, _ := m.scan(nil, data, true, func(string) {})
		s := newScrub(&cutReader{data: data, cuts: cuts}, m, &hitSet{})
		var got []byte
		buf := make([]byte, 16)
		for i := 0; ; i++ {
			size := 1 + i%len(buf)
			if len(cuts) > 0 {
				size = int(cuts[i%len(cuts)])%len(buf) + 1
			}
			n, err := s.Read(buf[:size])
			got = append(got, buf[:n]...)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		if !bytes.Equal(got, whole) {
			t.Errorf("split output differs from the whole read (%d bytes, want %d)", len(got), len(whole))
		}
	})
}
