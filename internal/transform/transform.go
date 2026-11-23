// Package transform changes requests on their way to an upstream and the
// answers on their way back. The swap transform puts the real values in place
// of placeholders in a request and takes them out of the answer again.
package transform

import (
	"fmt"
	"io"
	"net/http"

	"github.com/abrose/sbx-broker/internal/config"
	"github.com/abrose/sbx-broker/internal/secrets"
)

// Secrets gives the values of one namespace. The server gives each listener
// its current *secrets.Store plus the values the namespace retired.
type Secrets interface {
	// Lookup returns a copy of the value of an ok secret, or its state.
	Lookup(name string) ([]byte, secrets.State)
	// Values returns copies of the ok values, by secret name.
	Values() map[string][]byte
	// Retired returns copies of the values that left the ok state (a failed
	// or changing refresh, a reload, a secret removed from the file), by
	// secret name, several per name when they differ. An upstream may have
	// stored such a value, so the answer scrub still takes it out.
	Retired() map[string][][]byte
}

// Refusal is a request the broker does not forward. Status is 403 or 503 and
// Body is the JSON answer. It never holds a value.
type Refusal struct {
	Status int
	Body   map[string]string
}

func (r *Refusal) Error() string { return fmt.Sprintf("%d %s", r.Status, r.Body["error"]) }

func otherNamespace(ns string) *Refusal {
	return &Refusal{Status: http.StatusForbidden, Body: map[string]string{
		"error": "placeholder of another namespace", "namespace": ns,
	}}
}

func notLoaded(name string, st secrets.State) *Refusal {
	return &Refusal{Status: http.StatusServiceUnavailable, Body: map[string]string{
		"error": "secret not loaded", "secret": name, "state": string(st),
	}}
}

// Result tells what a transform did to a request.
type Result struct {
	Swapped   []string // the names of the secrets put in, sorted, each once
	Unscanned bool     // the body went on without a scan
	// Used holds a copy of each value put in, by secret name, for the
	// scrub of the answer (ruling R8). The caller wipes it after done runs.
	Used map[string][]byte
}

// Transform changes one request and its response.
type Transform interface {
	Name() string
	// Request changes r in place. On an error, r is unchanged; the error
	// may be a *Refusal.
	Request(r *http.Request, ns string, sec Secrets) (Result, error)
	// Response changes resp in place. It scrubs the values of used (the
	// Used of the request) and of sec. done gets the names of the secrets
	// taken out, exactly once: when the body reaches EOF or is closed, or
	// before Response returns an error (it closes the body then).
	Response(resp *http.Response, ns string, sec Secrets, used map[string][]byte, done func(scrubbed []string)) error
}

// New returns the transform that spec names. Only swap exists.
func New(spec config.TransformSpec) (Transform, error) {
	switch spec.Name {
	case "swap":
		return swap{}, nil
	}
	return nil, fmt.Errorf("unknown transform %s", spec.Name)
}

// NewScrubReader returns a reader that gives the text of r with each pattern
// (a key of patterns) replaced by its replacement, longest pattern first at
// each position. The func returns the replacements made so far, sorted.
// Close closes r when r is an io.Closer.
func NewScrubReader(r io.Reader, patterns map[string]string) (io.ReadCloser, func() []string) {
	pats := make([]pattern, 0, len(patterns))
	for from, to := range patterns {
		pats = append(pats, pattern{from: from, to: to, tag: to})
	}
	s := newScrub(r, newMatcher(pats), &hitSet{})
	return s, s.hits.list
}
