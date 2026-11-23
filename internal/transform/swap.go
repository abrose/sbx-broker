package transform

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/abrose/sbx-broker/internal/placeholder"
	"github.com/abrose/sbx-broker/internal/secrets"
)

// maxScanBody is the largest request body that is scanned.
const maxScanBody = 1 << 20

// swap puts the real values in place of the placeholders of a request and
// takes them out of the response.
type swap struct{}

func (swap) Name() string { return "swap" }

// Request scans every place first, then refuses or swaps. A refusal leaves r
// as it was: nothing is written before every placeholder is known to be ok.
func (swap) Request(r *http.Request, ns string, sec Secrets) (Result, error) {
	var s scan
	s.headers(r.Header)
	s.add(r.URL.EscapedPath(), pathEscape, func(p string) { setEscapedPath(r.URL, p) })
	s.add(r.URL.RawQuery, queryEscape, func(q string) { r.URL.RawQuery = q })
	unscanned, err := s.body(r)
	if err != nil {
		return Result{}, err
	}
	vals, refusal := s.resolve(ns, sec)
	if refusal != nil {
		return Result{}, refusal
	}
	s.apply(vals)
	// The copies from Lookup go to the caller for the scrub (ruling R8).
	return Result{Swapped: s.names(), Unscanned: unscanned, Used: vals}, nil
}

// spot is a text of the request that holds placeholders, with the escape a
// value needs in that place and the func that writes the swapped text back.
type spot struct {
	text string
	refs []placeholder.Ref
	esc  func([]byte) string
	set  func(string)
}

// scan collects the spots of a request in scan order: headers sorted by
// name, then the path, the query and the body.
type scan struct {
	spots  []spot
	commit []func() // run after every spot is set
}

func (s *scan) add(text string, esc func([]byte) string, set func(string)) bool {
	refs := placeholder.Find(text)
	if len(refs) == 0 {
		return false
	}
	s.spots = append(s.spots, spot{text: text, refs: refs, esc: esc, set: set})
	return true
}

// headers scans each token of each header value: as text, or else as Base64
// text that decodes to placeholders. Header names are never touched.
func (s *scan) headers(h http.Header) {
	for _, name := range slices.Sorted(maps.Keys(h)) {
		values := h[name]
		for j, v := range values {
			pieces := splitTokens(v)
			found := false
			for i, tok := range pieces {
				if s.add(tok, asIs, func(t string) { pieces[i] = t }) {
					found = true
					continue
				}
				if enc, dec, ok := decodeToken(tok); ok {
					found = s.add(dec, asIs, func(t string) { pieces[i] = enc.EncodeToString([]byte(t)) }) || found
				}
			}
			if found {
				s.commit = append(s.commit, func() { values[j] = strings.Join(pieces, "") })
			}
		}
	}
}

// splitTokens cuts v into tokens and runs of separators (spaces and commas),
// so that joining the pieces gives v again.
func splitTokens(v string) []string {
	var pieces []string
	start := 0
	for i := 1; i <= len(v); i++ {
		if i == len(v) || isSep(v[i]) != isSep(v[start]) {
			pieces = append(pieces, v[start:i])
			start = i
		}
	}
	return pieces
}

func isSep(b byte) bool { return b == ' ' || b == ',' }

var (
	base64Token = regexp.MustCompile(`^[A-Za-z0-9+/_-]+={0,2}$`)
	encodings   = []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	}
)

// decodeToken returns the first encoding that decodes tok to UTF-8 text with
// the placeholder marker, and that text.
func decodeToken(tok string) (*base64.Encoding, string, bool) {
	if len(tok) < 8 || !base64Token.MatchString(tok) {
		return nil, "", false
	}
	for _, enc := range encodings {
		d, err := enc.DecodeString(tok)
		if err == nil && utf8.Valid(d) && bytes.Contains(d, []byte(placeholder.Marker)) {
			return enc, string(d), true
		}
	}
	return nil, "", false
}

// setEscapedPath sets the path of u from its escaped form. RawPath stays only
// when the plain escape of Path would differ (for example a %2F).
func setEscapedPath(u *url.URL, escaped string) {
	// Cannot fail: the old escaped path was valid, a placeholder holds no
	// %, and url.PathEscape gives valid escapes.
	p, _ := url.PathUnescape(escaped)
	u.Path, u.RawPath = p, escaped
	if (&url.URL{Path: p}).EscapedPath() == escaped {
		u.RawPath = ""
	}
}

// body reads a body that may be scanned and adds it as a spot. The body can
// be read again afterwards, from the first byte. It reports whether the body
// goes on without a scan.
func (s *scan) body(r *http.Request) (bool, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return false, nil
	}
	esc, ok := bodyEscape(r.Header)
	if !ok || r.ContentLength > maxScanBody {
		return true, nil
	}
	orig := r.Body
	data, err := io.ReadAll(io.LimitReader(orig, maxScanBody+1))
	if err != nil {
		return false, fmt.Errorf("read request body: %w", err)
	}
	if len(data) > maxScanBody {
		r.Body = readCloser{io.MultiReader(bytes.NewReader(data), orig), orig}
		return true, nil
	}
	r.Body = readCloser{bytes.NewReader(data), orig}
	s.add(string(data), esc, func(b string) { setBody(r, b, orig) })
	return false, nil
}

// bodyEscape returns the escape for a value in a body of this type, or false
// when the body may not be scanned.
func bodyEscape(h http.Header) (func([]byte) string, bool) {
	if ce := strings.TrimSpace(h.Get("Content-Encoding")); ce != "" && !strings.EqualFold(ce, "identity") {
		return nil, false
	}
	mt, _, err := mime.ParseMediaType(h.Get("Content-Type"))
	switch {
	case err != nil:
		return nil, false
	case mt == "application/x-www-form-urlencoded":
		return queryEscape, true
	case mt == "application/json":
		return jsonEscape, true
	case strings.HasPrefix(mt, "text/"):
		return asIs, true
	}
	return nil, false
}

func setBody(r *http.Request, b string, orig io.Closer) {
	data := []byte(b)
	r.Body = readCloser{bytes.NewReader(data), orig}
	r.ContentLength = int64(len(data))
	if r.Header.Get("Content-Length") != "" {
		r.Header.Set("Content-Length", strconv.Itoa(len(data)))
	}
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
}

// readCloser reads from a replacement and closes the original body.
type readCloser struct {
	io.Reader
	io.Closer
}

func asIs(v []byte) string        { return string(v) }
func pathEscape(v []byte) string  { return url.PathEscape(string(v)) }
func queryEscape(v []byte) string { return url.QueryEscape(string(v)) }

// jsonEscape returns v as the inside of a JSON string, without HTML escapes.
func jsonEscape(v []byte) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(string(v)) // a string always encodes
	out := b.Bytes()
	return string(out[1 : len(out)-2]) // drop the quotes and the newline
}

// resolve refuses a placeholder of another namespace first (403), then the
// first placeholder whose secret is not ok (503), both in scan order.
// Otherwise it returns the value of each secret.
func (s *scan) resolve(ns string, sec Secrets) (map[string][]byte, *Refusal) {
	for _, sp := range s.spots {
		for _, ref := range sp.refs {
			if ref.Namespace != ns {
				return nil, otherNamespace(ref.Namespace)
			}
		}
	}
	vals := map[string][]byte{}
	for _, sp := range s.spots {
		for _, ref := range sp.refs {
			if _, ok := vals[ref.Secret]; ok {
				continue
			}
			v, st := sec.Lookup(ref.Secret)
			if st != secrets.StateOK {
				wipe(vals)
				return nil, notLoaded(ref.Secret, st)
			}
			vals[ref.Secret] = v
		}
	}
	return vals, nil
}

func (s *scan) apply(vals map[string][]byte) {
	for _, sp := range s.spots {
		var b strings.Builder
		last := 0
		for _, ref := range sp.refs {
			b.WriteString(sp.text[last:ref.Start])
			b.WriteString(sp.esc(vals[ref.Secret]))
			last = ref.End
		}
		b.WriteString(sp.text[last:])
		sp.set(b.String())
	}
	for _, c := range s.commit {
		c()
	}
}

func (s *scan) names() []string {
	var names []string
	for _, sp := range s.spots {
		for _, ref := range sp.refs {
			names = append(names, ref.Secret)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// wipe zeroes the copies of the values a refused request fetched.
func wipe(vals map[string][]byte) {
	for _, v := range vals {
		clear(v)
	}
}

// Response scrubs every header value and the body, decoded from gzip or
// deflate first: each value that went into the request, each ok value and
// each retired value of the namespace, in each form the broker sends (see
// responsePatterns), becomes the same form of its placeholder.
func (swap) Response(resp *http.Response, ns string, sec Secrets, used map[string][]byte, done func(scrubbed []string)) error {
	current, retired := sec.Values(), sec.Retired()
	m := newMatcher(responsePatterns(ns, valueSets(used, current, retired)))
	wipe(current) // the copies of sec; used belongs to the caller
	for _, vs := range retired {
		for _, v := range vs {
			clear(v)
		}
	}
	hits := &hitSet{}
	// The headers first, on every path: Content-Encoding may echo a value,
	// and the error below quotes it.
	scrubHeader(resp.Header, m, hits)
	body := resp.Body
	if body == nil {
		body = http.NoBody
	}
	body, empty := peekEmpty(resp.Header, body)
	decoded := body
	if !empty {
		var err error
		if decoded, err = decodeBody(resp.Header, body); err != nil {
			_ = body.Close()
			if done != nil {
				done(hits.list())
			}
			return err
		}
	}
	if decoded != body {
		resp.Header.Del("Content-Encoding")
		resp.Uncompressed = true
	}
	s := newScrub(decoded, m, hits)
	// The transport fills the trailers just before the body gives EOF.
	s.atEOF = func() { scrubHeader(resp.Trailer, m, hits) }
	s.onDone = func() {
		if done != nil {
			done(hits.list())
		}
	}
	resp.Body = s
	if empty {
		return nil // no bytes to change: Content-Length stays as it is
	}
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	return nil
}

// peekEmpty reports a body with no bytes, decided by the bytes alone: status,
// method and Content-Length do not count, so Response does not depend on the
// caller's transport. An empty body is never decoded, and its headers
// (Content-Encoding and Content-Length describe the resource) stay as they
// are. It peeks only when a Content-Encoding asks for decoding: a plain
// stream (such as SSE) must not wait here for its first byte. The returned
// body gives the peeked byte too and closes the original.
func peekEmpty(h http.Header, body io.ReadCloser) (io.ReadCloser, bool) {
	if body == http.NoBody {
		return body, true
	}
	if identity(contentEncoding(h)) {
		return body, false
	}
	br := bufio.NewReader(body)
	_, err := br.Peek(1)
	return readCloser{br, body}, err == io.EOF
}

func contentEncoding(h http.Header) string {
	return strings.TrimSpace(strings.Join(h.Values("Content-Encoding"), ", "))
}

func identity(ce string) bool { return ce == "" || strings.EqualFold(ce, "identity") }

// decodeBody returns body, or a reader that decodes it for a gzip or
// deflate Content-Encoding (ruling R7): compressed bytes would hide a value
// from the scrub. Any other encoding is an error.
func decodeBody(h http.Header, body io.ReadCloser) (io.ReadCloser, error) {
	ce := contentEncoding(h)
	if identity(ce) {
		return body, nil
	}
	switch strings.ToLower(ce) {
	case "gzip":
		return &decoder{src: body, open: func(r io.Reader) (io.Reader, error) { return gzip.NewReader(r) }}, nil
	case "deflate":
		return &decoder{src: body, open: openDeflate}, nil
	}
	return nil, fmt.Errorf("unsupported content encoding %s", ce)
}

// openDeflate reads HTTP deflate: zlib-wrapped as RFC 9110 defines it, or
// raw deflate, which some servers send. A zlib header has compression method
// 8 in its low nibble, and the two header bytes are a multiple of 31.
func openDeflate(r io.Reader) (io.Reader, error) {
	br := bufio.NewReader(r)
	if h, err := br.Peek(2); err == nil && h[0]&0x0f == 8 && (uint16(h[0])<<8|uint16(h[1]))%31 == 0 {
		return zlib.NewReader(br)
	}
	return flate.NewReader(br), nil
}

// decoder opens its decoding reader at the first Read, so Response never
// waits for body bytes. Close closes the original body.
type decoder struct {
	src  io.ReadCloser
	open func(io.Reader) (io.Reader, error)
	r    io.Reader
}

func (d *decoder) Read(p []byte) (int, error) {
	if d.r == nil {
		r, err := d.open(d.src) // not empty: Response peeked a byte
		if err != nil {
			return 0, err
		}
		d.r = r
	}
	return d.r.Read(p)
}

func (d *decoder) Close() error { return d.src.Close() }

func scrubHeader(h http.Header, m *matcher, hits *hitSet) {
	for _, values := range h {
		for j, v := range values {
			out, _ := m.scan(nil, []byte(v), true, hits.add)
			values[j] = string(out)
		}
	}
}
