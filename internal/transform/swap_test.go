package transform

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/abrose/sbx-broker/internal/config"
	"github.com/abrose/sbx-broker/internal/secrets"
)

var _ Secrets = (*secrets.Store)(nil)

const (
	apiKey = "real-api-key-1"
	pass   = "p@ss word/1+&"
	odd    = `o"dd<v>\al`
)

// testValues are the ok values of the fake namespace t1, by secret name.
var testValues = map[string]string{"api-key": apiKey, "pass": pass, "odd": odd}

type fakeSecret struct {
	value string
	state secrets.State
}

type fakeSecrets map[string]fakeSecret

func (f fakeSecrets) Lookup(name string) ([]byte, secrets.State) {
	s, ok := f[name]
	if !ok {
		return nil, secrets.StateUndefined
	}
	if s.state != secrets.StateOK {
		return nil, s.state
	}
	return []byte(s.value), secrets.StateOK
}

func (f fakeSecrets) Values() map[string][]byte {
	out := map[string][]byte{}
	for name, s := range f {
		if s.state == secrets.StateOK {
			out[name] = []byte(s.value)
		}
	}
	return out
}

func t1() fakeSecrets {
	f := fakeSecrets{
		"slow":   {state: secrets.StatePending},
		"broken": {state: secrets.StateFailed},
	}
	for name, v := range testValues {
		f[name] = fakeSecret{value: v, state: secrets.StateOK}
	}
	return f
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// show makes a text safe for a failure message: it names the test values
// instead of printing them, and hides the text when a value is still in it.
func show(s string) string {
	names := slices.Sorted(maps.Keys(testValues))
	for _, name := range names {
		s = strings.ReplaceAll(s, testValues[name], "<"+name+">")
		s = strings.ReplaceAll(s, b64(testValues[name]), "<b64 "+name+">")
	}
	if found := leaks(s); len(found) > 0 {
		return "(hidden: holds " + strings.Join(found, ", ") + ")"
	}
	return strconv.Quote(s)
}

func check(t *testing.T, what, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %s, want %s", what, show(got), show(want))
	}
}

func swapT1(t *testing.T, r *http.Request) (Result, error) {
	t.Helper()
	tr, err := New(config.TransformSpec{Name: "swap", Settings: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	return tr.Request(r, "t1", t1())
}

func mustSwap(t *testing.T, r *http.Request) Result {
	t.Helper()
	res, err := swapT1(t, r)
	if err != nil {
		t.Fatalf("swap: %v", err)
	}
	return res
}

func bodyOf(t *testing.T, r *http.Request) string {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func get(url string) *http.Request { return httptest.NewRequest(http.MethodGet, url, nil) }

func post(url, contentType, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, url, strings.NewReader(body))
	r.Header.Set("Content-Type", contentType)
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return r
}

func swapped(t *testing.T, res Result, want ...string) {
	t.Helper()
	if !slices.Equal(res.Swapped, want) {
		t.Errorf("Swapped = %v, want %v", res.Swapped, want)
	}
}

func TestNewKnowsOnlySwap(t *testing.T) {
	tr, err := New(config.TransformSpec{Name: "swap"})
	if err != nil || tr.Name() != "swap" {
		t.Fatalf("New(swap) = %v, %v", tr, err)
	}
	_, err = New(config.TransformSpec{Name: "rot13"})
	if err == nil || err.Error() != "unknown transform rot13" {
		t.Fatalf("New(rot13) error = %v", err)
	}
}

func TestSwapHeader(t *testing.T) {
	r := get("http://api.test/x")
	r.Header.Set("Authorization", "Bearer sbxb_t1_api_key")
	res := mustSwap(t, r)
	check(t, "Authorization", r.Header.Get("Authorization"), "Bearer "+apiKey)
	swapped(t, res, "api-key")
	if res.Unscanned {
		t.Error("Unscanned for a request with no body")
	}
}

func TestSwapBase64Header(t *testing.T) {
	r := get("http://api.test/x")
	r.Header.Set("X-API-Key", b64("sbxb_t1_api_key"))
	res := mustSwap(t, r)
	check(t, "X-API-Key", r.Header.Get("X-API-Key"), b64(apiKey))
	swapped(t, res, "api-key")
}

func TestSwapBasic(t *testing.T) {
	r := get("http://api.test/x")
	r.Header.Set("Authorization", "Basic "+b64("user:sbxb_t1_pass"))
	res := mustSwap(t, r)
	check(t, "Authorization", r.Header.Get("Authorization"), "Basic "+b64("user:"+pass))
	swapped(t, res, "pass")
}

func TestSwapBase64KeepsAlphabetAndPadding(t *testing.T) {
	cases := []struct {
		name string
		enc  *base64.Encoding
	}{
		{"std", base64.StdEncoding},
		{"raw std", base64.RawStdEncoding},
		{"url", base64.URLEncoding},
		{"raw url", base64.RawURLEncoding},
	}
	// "???" encodes to "Pz8/" or "Pz8_", so the token shows its alphabet,
	// and 17 bytes need padding, so it shows its padding. A token without
	// these hints decodes as Std, the first encoding tried.
	for _, c := range cases {
		r := get("http://api.test/x")
		r.Header.Set("X-K", "a,"+c.enc.EncodeToString([]byte("??? sbxb_t1_odd x"))+" b")
		mustSwap(t, r)
		check(t, c.name, r.Header.Get("X-K"), "a,"+c.enc.EncodeToString([]byte("??? "+odd+" x"))+" b")
	}
}

func TestSwapMultiValueHeader(t *testing.T) {
	r := get("http://api.test/x")
	r.Header.Add("X-K", "sbxb_t1_api_key")
	r.Header.Add("X-K", "a, "+b64("sbxb_t1_pass")+",sbxb_t1_api_key")
	res := mustSwap(t, r)
	got := r.Header.Values("X-K")
	if len(got) != 2 {
		t.Fatalf("got %d values", len(got))
	}
	check(t, "first", got[0], apiKey)
	check(t, "second", got[1], "a, "+b64(pass)+","+apiKey)
	swapped(t, res, "api-key", "pass")
}

func TestSwapLeavesNamesAndHost(t *testing.T) {
	r := get("http://sbxb_t1_api_key.test/x")
	r.Header["sbxb_t1_api_key"] = []string{"plain"}
	res := mustSwap(t, r)
	check(t, "Host", r.Host, "sbxb_t1_api_key.test")
	if v := r.Header["sbxb_t1_api_key"]; len(v) != 1 || v[0] != "plain" {
		t.Errorf("header named after a placeholder = %v", v)
	}
	swapped(t, res)
}

func TestSwapQuery(t *testing.T) {
	r := get("http://api.test/x?key=sbxb_t1_pass&x=1")
	res := mustSwap(t, r)
	check(t, "query", r.URL.RawQuery, "key=p%40ss+word%2F1%2B%26&x=1")
	swapped(t, res, "pass")
}

func TestSwapInPath(t *testing.T) {
	r := get("http://api.test/a/sbxb_t1_api_key/b")
	res := mustSwap(t, r)
	check(t, "path", r.URL.Path, "/a/"+apiKey+"/b")
	check(t, "escaped path", r.URL.EscapedPath(), "/a/"+apiKey+"/b")
	if r.URL.RawPath != "" {
		t.Errorf("RawPath = %s, want empty", show(r.URL.RawPath))
	}
	swapped(t, res, "api-key")
}

func TestSwapInEscapedPath(t *testing.T) {
	cases := []struct{ name, url, path, escaped string }{
		{"value needs escapes", "http://api.test/v/sbxb_t1_pass",
			"/v/" + pass, "/v/p@ss%20word%2F1+&"},
		{"path already escaped", "http://api.test/a%2Fb/sbxb_t1_pass/c",
			"/a/b/" + pass + "/c", "/a%2Fb/p@ss%20word%2F1+&/c"},
		{"escaped path, plain value", "http://api.test/a%2Fb/sbxb_t1_api_key",
			"/a/b/" + apiKey, "/a%2Fb/" + apiKey},
	}
	for _, c := range cases {
		r := get(c.url)
		mustSwap(t, r)
		check(t, c.name+": path", r.URL.Path, c.path)
		check(t, c.name+": escaped path", r.URL.EscapedPath(), c.escaped)
		check(t, c.name+": URL", r.URL.String(), "http://api.test"+c.escaped)
	}
}

func TestSwapFormBody(t *testing.T) {
	r := post("http://api.test/token", "application/x-www-form-urlencoded",
		"grant_type=client_credentials&client_id=c1&client_secret=sbxb_t1_pass")
	res := mustSwap(t, r)
	want := "grant_type=client_credentials&client_id=c1&client_secret=p%40ss+word%2F1%2B%26"
	check(t, "body", bodyOf(t, r), want)
	if r.ContentLength != int64(len(want)) {
		t.Errorf("ContentLength = %d, want %d", r.ContentLength, len(want))
	}
	check(t, "Content-Length", r.Header.Get("Content-Length"), strconv.Itoa(len(want)))
	if r.GetBody == nil {
		t.Fatal("GetBody is nil")
	}
	again, _ := r.GetBody()
	b, _ := io.ReadAll(again)
	check(t, "GetBody", string(b), want)
	swapped(t, res, "pass")
}

func TestSwapJSONBody(t *testing.T) {
	r := post("http://api.test/x", "application/json; charset=utf-8", `{"k":"sbxb_t1_pass","n":1}`)
	mustSwap(t, r)
	check(t, "body", bodyOf(t, r), `{"k":"p@ss word/1+&","n":1}`)

	r = post("http://api.test/x", "application/json", `{"k":"sbxb_t1_odd"}`)
	mustSwap(t, r)
	check(t, "body with escapes", bodyOf(t, r), `{"k":"o\"dd<v>\\al"}`)
}

func TestSwapTextBody(t *testing.T) {
	r := post("http://api.test/x", "text/plain", "token sbxb_t1_odd\n")
	res := mustSwap(t, r)
	check(t, "body", bodyOf(t, r), "token "+odd+"\n")
	swapped(t, res, "odd")
}

func TestSwapLeavesForeignBase64Alone(t *testing.T) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	cursor := base64.StdEncoding.EncodeToString(random)
	token := base64.URLEncoding.EncodeToString([]byte("hello-world-token"))
	r := get("http://api.test/x")
	r.Header.Set("X-Cursor", cursor)
	r.Header.Set("X-Token", token)
	res := mustSwap(t, r)
	check(t, "X-Cursor", r.Header.Get("X-Cursor"), cursor)
	check(t, "X-Token", r.Header.Get("X-Token"), token)
	swapped(t, res)
}

func refusal(t *testing.T, err error) *Refusal {
	t.Helper()
	var ref *Refusal
	if !errors.As(err, &ref) {
		t.Fatalf("error = %v, want a *Refusal", err)
	}
	return ref
}

func TestSwapRefusesOtherNamespace(t *testing.T) {
	body := "a=1&b=sbxb_t2_api_key"
	r := post("http://api.test/x?key=sbxb_t1_pass", "application/x-www-form-urlencoded", body)
	r.Header.Set("Authorization", "Bearer sbxb_t1_api_key")
	_, err := swapT1(t, r)
	ref := refusal(t, err)
	want := map[string]string{"error": "placeholder of another namespace", "namespace": "t2"}
	if ref.Status != http.StatusForbidden || !maps.Equal(ref.Body, want) {
		t.Errorf("refusal = %d %v", ref.Status, ref.Body)
	}
	check(t, "body", bodyOf(t, r), body)
	check(t, "Authorization", r.Header.Get("Authorization"), "Bearer sbxb_t1_api_key")
	check(t, "query", r.URL.RawQuery, "key=sbxb_t1_pass")
	if r.ContentLength != int64(len(body)) {
		t.Errorf("ContentLength = %d", r.ContentLength)
	}
}

func TestSwapRefusesNotLoaded(t *testing.T) {
	cases := []struct{ placeholder, secret, state string }{
		{"sbxb_t1_slow", "slow", "pending"},
		{"sbxb_t1_nope", "nope", "undefined"},
		{"sbxb_t1_broken", "broken", "failed"},
	}
	for _, c := range cases {
		r := get("http://api.test/x")
		r.Header.Set("Authorization", "Bearer "+c.placeholder)
		r.Header.Set("X-Other", "sbxb_t1_api_key")
		_, err := swapT1(t, r)
		ref := refusal(t, err)
		want := map[string]string{"error": "secret not loaded", "secret": c.secret, "state": c.state}
		if ref.Status != http.StatusServiceUnavailable || !maps.Equal(ref.Body, want) {
			t.Errorf("%s: refusal = %d %v", c.secret, ref.Status, ref.Body)
		}
		check(t, c.secret+": X-Other", r.Header.Get("X-Other"), "sbxb_t1_api_key")
	}
}

func TestSwapRefusalOrder(t *testing.T) {
	cases := []struct {
		name   string
		req    func() *http.Request
		status int
		key    string // the namespace (403) or the secret (503)
	}{
		{"another namespace wins over not loaded", func() *http.Request {
			r := post("http://api.test/sbxb_t1_slow", "text/plain", "x sbxb_t2_k1")
			r.Header.Set("A", "sbxb_t1_nope")
			return r
		}, 403, "t2"},
		{"headers in name order", func() *http.Request {
			r := get("http://api.test/x")
			r.Header.Set("B-Header", "sbxb_t1_slow")
			r.Header.Set("A-Header", "sbxb_t1_nope")
			return r
		}, 503, "nope"},
		{"headers before path", func() *http.Request {
			r := get("http://api.test/sbxb_t1_slow")
			r.Header.Set("X", "sbxb_t1_nope")
			return r
		}, 503, "nope"},
		{"path before query", func() *http.Request {
			return get("http://api.test/sbxb_t1_slow?k=sbxb_t1_nope")
		}, 503, "slow"},
		{"query before body", func() *http.Request {
			return post("http://api.test/x?k=sbxb_t1_nope", "text/plain", "sbxb_t1_slow")
		}, 503, "nope"},
	}
	for _, c := range cases {
		_, err := swapT1(t, c.req())
		ref := refusal(t, err)
		key := ref.Body["secret"]
		if c.status == 403 {
			key = ref.Body["namespace"]
		}
		if ref.Status != c.status || key != c.key {
			t.Errorf("%s: refusal = %d %v", c.name, ref.Status, ref.Body)
		}
	}
}

// hideLength hides the size of a body so the request has ContentLength -1.
type hideLength struct{ io.Reader }

func TestSwapUnscanned(t *testing.T) {
	big := "sbxb_t1_api_key " + strings.Repeat("a", 1<<20+1-len("sbxb_t1_api_key "))
	cases := []struct {
		name string
		req  func() *http.Request
		body string
	}{
		{"text over 1 MiB", func() *http.Request {
			return post("http://api.test/x", "text/plain", big)
		}, big},
		{"text over 1 MiB, length unknown", func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, "http://api.test/x", hideLength{strings.NewReader(big)})
			r.Header.Set("Content-Type", "text/plain")
			return r
		}, big},
		{"octet-stream", func() *http.Request {
			return post("http://api.test/x", "application/octet-stream", "k=sbxb_t1_api_key")
		}, "k=sbxb_t1_api_key"},
		{"no content type", func() *http.Request {
			r := post("http://api.test/x", "", "k=sbxb_t1_api_key")
			r.Header.Del("Content-Type")
			return r
		}, "k=sbxb_t1_api_key"},
		{"gzip form", func() *http.Request {
			r := post("http://api.test/x", "application/x-www-form-urlencoded", "k=sbxb_t1_api_key")
			r.Header.Set("Content-Encoding", "gzip")
			return r
		}, "k=sbxb_t1_api_key"},
	}
	for _, c := range cases {
		r := c.req()
		res, err := swapT1(t, r)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !res.Unscanned {
			t.Errorf("%s: not Unscanned", c.name)
		}
		swapped(t, res)
		if got := bodyOf(t, r); got != c.body {
			t.Errorf("%s: body changed (%d bytes, want %d)", c.name, len(got), len(c.body))
		}
	}
}

func TestSwapScansUpTo1MiB(t *testing.T) {
	body := strings.Repeat("a", 1<<20-len(" sbxb_t1_api_key")) + " sbxb_t1_api_key"
	r := httptest.NewRequest(http.MethodPost, "http://api.test/x", hideLength{strings.NewReader(body)})
	r.Header.Set("Content-Type", "text/plain")
	r.Header.Set("Content-Encoding", "identity")
	res := mustSwap(t, r)
	if res.Unscanned {
		t.Error("a body of exactly 1 MiB is Unscanned")
	}
	got := bodyOf(t, r)
	if !strings.HasSuffix(got, apiKey) || len(got) != len(body)-len("sbxb_t1_api_key")+len(apiKey) {
		t.Errorf("body not swapped (%d bytes)", len(got))
	}
	if r.ContentLength != int64(len(got)) {
		t.Errorf("ContentLength = %d, want %d", r.ContentLength, len(got))
	}
}

func TestSwapClosesTheOriginalBody(t *testing.T) {
	orig := &closeCounter{Reader: strings.NewReader("k=sbxb_t1_api_key")}
	r := httptest.NewRequest(http.MethodPost, "http://api.test/x", orig)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mustSwap(t, r)
	if err := r.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if orig.closed != 1 {
		t.Errorf("original body closed %d times, want 1", orig.closed)
	}
}

type closeCounter struct {
	io.Reader
	closed int
}

func (c *closeCounter) Close() error { c.closed++; return nil }
