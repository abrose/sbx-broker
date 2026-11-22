package transform

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/abrose/sbx-broker/internal/placeholder"
)

// pattern is a text to replace. tag names what was replaced: the secret
// name, or for NewScrubReader the replacement.
type pattern struct{ from, to, tag string }

// wholeForms are the encodings the broker sends a value in (rulings R4 and
// R7): as text, escaped for a query or form, a path, a JSON string, and as
// Base64 in both alphabets. Then the common re-encodings an upstream may
// make (ruling R9): JSON with HTML escapes, JSON with \/, the
// encodeURIComponent form, and each percent form with lowercase hex.
var wholeForms = []func([]byte) string{
	asIs, queryEscape, pathEscape, jsonEscape,
	base64.StdEncoding.EncodeToString, base64.URLEncoding.EncodeToString,
	jsonHTMLEscape, jsonSlashEscape, uriComponent,
	func(v []byte) string { return lowerHex(queryEscape(v)) },
	func(v []byte) string { return lowerHex(pathEscape(v)) },
	func(v []byte) string { return lowerHex(uriComponent(v)) },
}

// jsonHTMLEscape is the inside of a JSON string as Go's json.Marshal writes
// it: &, < and > as &, < and >.
func jsonHTMLEscape(v []byte) string {
	b, _ := json.Marshal(string(v)) // a string always encodes
	return string(b[1 : len(b)-1])
}

// jsonSlashEscape is the inside of a JSON string with / as \/ (as PHP's
// json_encode writes it).
func jsonSlashEscape(v []byte) string {
	return strings.ReplaceAll(jsonEscape(v), "/", `\/`)
}

// uriComponent is JavaScript's encodeURIComponent: every byte but
// A-Z a-z 0-9 - _ . ! ~ * ' ( ) as %XX (space as %20).
func uriComponent(v []byte) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for _, c := range v {
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0f])
	}
	return b.String()
}

// lowerHex writes the two hex digits after each % in lowercase.
func lowerHex(s string) string {
	b := []byte(s)
	for i := 0; i+2 < len(b); i++ {
		if b[i] == '%' {
			b[i+1], b[i+2] = toLower(b[i+1]), toLower(b[i+2])
			i += 2
		}
	}
	return string(b)
}

func toLower(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

// responsePatterns returns, for each value, the patterns that turn it into
// its placeholder: each whole form of the value into the same form of the
// placeholder, and the core of its Base64 in both alphabets at each of the
// three byte offsets (ruling R4), so a value inside a longer Base64 text is
// caught too.
func responsePatterns(ns string, values map[string][][]byte) []pattern {
	names := slices.Sorted(maps.Keys(values))
	var pats []pattern
	// The whole patterns come first: a core can be the same text as a whole
	// Base64, and newMatcher keeps the first pattern of a text.
	for _, name := range names {
		ph := []byte(placeholder.Format(ns, name))
		for _, v := range values[name] {
			for _, form := range wholeForms {
				pats = append(pats, pattern{from: form(v), to: form(ph), tag: name})
			}
		}
	}
	for _, name := range names {
		ph := []byte(placeholder.Format(ns, name))
		for _, v := range values[name] {
			for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding} {
				for k := range 3 {
					pats = append(pats, pattern{from: base64Core(enc, v, k), to: base64Core(enc, ph, k), tag: name})
				}
			}
		}
	}
	return pats
}

// valueSets returns the values to scrub, by name: the union of the values
// that went into the request and the current ones (ruling R8). A name keeps
// both when a refresh changed its value in between.
func valueSets(used, current map[string][]byte) map[string][][]byte {
	out := map[string][][]byte{}
	for _, m := range []map[string][]byte{used, current} {
		for name, v := range m {
			if !slices.ContainsFunc(out[name], func(w []byte) bool { return bytes.Equal(v, w) }) {
				out[name] = append(out[name], v)
			}
		}
	}
	return out
}

// base64Core returns the characters of the Base64 (in the alphabet of e) of
// k zero bytes and v whose 6 bits all come from bytes of v. The bits of v
// run from bit 8k to bit 8(k+len(v)); character j holds bits 6j to 6j+6.
func base64Core(e *base64.Encoding, v []byte, k int) string {
	enc := e.EncodeToString(append(make([]byte, k), v...))
	from := (8*k + 5) / 6      // the first character that starts at or after bit 8k
	to := 8 * (k + len(v)) / 6 // the end of the last character that ends at or before the last bit of v
	if to <= from {
		return ""
	}
	return enc[from:to]
}

// matcher finds the patterns in a text, leftmost first, longest first at
// each position.
type matcher struct {
	byFirst [256][]pattern // by first byte, longest first
}

// newMatcher drops empty patterns and keeps the first pattern of each text.
func newMatcher(pats []pattern) *matcher {
	m := &matcher{}
	seen := map[string]bool{}
	for _, p := range pats {
		if p.from == "" || seen[p.from] {
			continue
		}
		seen[p.from] = true
		m.byFirst[p.from[0]] = append(m.byFirst[p.from[0]], p)
	}
	for i := range m.byFirst {
		slices.SortFunc(m.byFirst[i], func(a, b pattern) int {
			return cmp.Or(cmp.Compare(len(b.from), len(a.from)), cmp.Compare(a.from, b.from))
		})
	}
	return m
}

// scan appends to dst the text of src with each match replaced, calls hit
// with the tag of each match, and returns how many bytes of src it used.
// Unless final, it stops at the first position whose rest is a proper prefix
// of a pattern: more input could still make a (longer) pattern match there.
// So each decision is the same as for the whole text at once.
func (m *matcher) scan(dst, src []byte, final bool, hit func(tag string)) ([]byte, int) {
	run, i := 0, 0
	for i < len(src) {
		cands := m.byFirst[src[i]]
		if len(cands) == 0 {
			i++
			continue
		}
		rest := src[i:]
		if !final && couldGrow(cands, rest) {
			break
		}
		if p, ok := longest(cands, rest); ok {
			dst = append(dst, src[run:i]...)
			dst = append(dst, p.to...)
			hit(p.tag)
			i += len(p.from)
			run = i
			continue
		}
		i++
	}
	return append(dst, src[run:i]...), i
}

// couldGrow reports whether rest is a proper prefix of a pattern.
func couldGrow(cands []pattern, rest []byte) bool {
	for _, p := range cands {
		if len(p.from) > len(rest) && p.from[:len(rest)] == string(rest) {
			return true
		}
	}
	return false
}

// longest returns the longest pattern that rest starts with.
func longest(cands []pattern, rest []byte) (pattern, bool) {
	for _, p := range cands {
		if len(p.from) <= len(rest) && string(rest[:len(p.from)]) == p.from {
			return p, true
		}
	}
	return pattern{}, false
}

// hitSet collects tags; safe for concurrent use.
type hitSet struct {
	mu   sync.Mutex
	tags map[string]bool
}

func (h *hitSet) add(tag string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.tags == nil {
		h.tags = map[string]bool{}
	}
	h.tags[tag] = true
}

func (h *hitSet) list() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Sorted(maps.Keys(h.tags))
}

var errClosed = errors.New("read after close")

const chunkSize = 32 << 10

// scrub is the reader of NewScrubReader. It keeps back only the bytes that
// could start a pattern, until more input decides them.
type scrub struct {
	in      io.Reader
	m       *matcher
	hits    *hitSet
	chunk   []byte
	pending []byte // input not yet decided
	buf     []byte // backing array of out
	out     []byte // output not yet returned
	err     error  // from in; set once
	closed  atomic.Bool
	once    sync.Once
	atEOF   func() // runs in Read when in gives io.EOF
	onDone  func() // runs once, at EOF or Close
}

func newScrub(in io.Reader, m *matcher, hits *hitSet) *scrub {
	return &scrub{in: in, m: m, hits: hits, chunk: make([]byte, chunkSize)}
}

func (s *scrub) Read(p []byte) (int, error) {
	if s.closed.Load() {
		return 0, errClosed
	}
	if len(p) == 0 {
		return 0, nil
	}
	for len(s.out) == 0 {
		if s.err != nil {
			if s.err == io.EOF {
				s.finish()
			}
			return 0, s.err
		}
		s.fill()
	}
	n := copy(p, s.out)
	s.out = s.out[n:]
	return n, nil
}

// fill reads once from in and decides what it can. At EOF it decides
// everything. On another error it drops the bytes it kept back: they may be
// the start of a value.
func (s *scrub) fill() {
	n, err := s.in.Read(s.chunk)
	s.pending = append(s.pending, s.chunk[:n]...)
	var used int
	s.buf, used = s.m.scan(s.buf[:0], s.pending, err == io.EOF, s.hits.add)
	s.out = s.buf
	s.pending = append(s.pending[:0], s.pending[used:]...)
	if err == nil {
		return
	}
	s.err, s.pending = err, nil
	if err == io.EOF && s.atEOF != nil {
		s.atEOF()
	}
}

func (s *scrub) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	var err error
	if c, ok := s.in.(io.Closer); ok {
		err = c.Close()
	}
	s.finish()
	return err
}

func (s *scrub) finish() {
	s.once.Do(func() {
		if s.onDone != nil {
			s.onDone()
		}
	})
}
