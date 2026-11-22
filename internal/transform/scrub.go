package transform

import (
	"cmp"
	"encoding/base64"
	"errors"
	"io"
	"maps"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/abrose/sbx-broker/internal/placeholder"
)

// pattern is a text to replace. tag names what was replaced: the secret
// name, or for NewScrubReader the replacement.
type pattern struct{ from, to, tag string }

// responsePatterns returns, for each value, the patterns that turn it into
// its placeholder: the value as text, its whole StdEncoding Base64, and the
// core of its Base64 at each of the three byte offsets (ruling R4), so a
// value inside a longer Base64 text is caught too.
func responsePatterns(ns string, values map[string][]byte) []pattern {
	names := slices.Sorted(maps.Keys(values))
	var pats []pattern
	// The whole patterns come first: a core can be the same text as a whole
	// Base64, and newMatcher keeps the first pattern of a text.
	for _, name := range names {
		v, ph := values[name], []byte(placeholder.Format(ns, name))
		pats = append(pats,
			pattern{from: string(v), to: string(ph), tag: name},
			pattern{from: base64.StdEncoding.EncodeToString(v), to: base64.StdEncoding.EncodeToString(ph), tag: name})
	}
	for _, name := range names {
		v, ph := values[name], []byte(placeholder.Format(ns, name))
		for k := range 3 {
			pats = append(pats, pattern{from: base64Core(v, k), to: base64Core(ph, k), tag: name})
		}
	}
	return pats
}

// base64Core returns the characters of the StdEncoding of k zero bytes and v
// whose 6 bits all come from bytes of v. The bits of v run from bit 8k to
// bit 8(k+len(v)); character j holds bits 6j to 6j+6.
func base64Core(v []byte, k int) string {
	enc := base64.StdEncoding.EncodeToString(append(make([]byte, k), v...))
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
