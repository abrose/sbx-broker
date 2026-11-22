// Package placeholder formats, validates and finds the credential placeholders
// (sbxb_<ns>_<name>) that apps send instead of real secrets.
package placeholder

import (
	"regexp"
	"strings"
)

// Marker starts every placeholder.
const Marker = "sbxb_"

// Ref is one placeholder found in a text. Secret is the secret name (with "-").
// Start and End are the byte offsets of the placeholder text.
type Ref struct {
	Namespace, Secret string
	Start, End        int
}

var (
	namespaceRE = regexp.MustCompile(`^[a-z0-9]{1,16}$`)
	secretRE    = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	aliasRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)
	// anchored at the marker; edges are checked by hand (no lookaround in RE2).
	placeholderRE = regexp.MustCompile(`^sbxb_([a-z0-9]{1,16})_([a-z0-9]+(?:_[a-z0-9]+)*)`)
)

const maxSecretLen = 41

// Format returns the placeholder of secret in namespace ns.
func Format(ns, secret string) string {
	return Marker + ns + "_" + strings.ReplaceAll(secret, "-", "_")
}

// ValidNamespace reports whether ns is a valid namespace name.
func ValidNamespace(ns string) bool { return namespaceRE.MatchString(ns) }

// ValidSecretName reports whether name is a valid secret name: no empty
// segment, at most 41 characters.
func ValidSecretName(name string) bool {
	return len(name) <= maxSecretLen && secretRE.MatchString(name)
}

// ValidAlias reports whether name is a valid route or database alias.
func ValidAlias(name string) bool { return aliasRE.MatchString(name) }

func isWord(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// Find returns all placeholders in s, in order. A placeholder must not touch
// a letter, digit or underscore on either side.
func Find(s string) []Ref {
	var refs []Ref
	pos := 0
	for pos < len(s) {
		i := strings.Index(s[pos:], Marker)
		if i < 0 {
			break
		}
		start := pos + i
		pos = start + 1
		if start > 0 && isWord(s[start-1]) {
			continue
		}
		m := placeholderRE.FindStringSubmatchIndex(s[start:])
		if m == nil {
			continue
		}
		end := start + m[1]
		if end < len(s) && isWord(s[end]) {
			continue
		}
		refs = append(refs, Ref{
			Namespace: s[start+m[2] : start+m[3]],
			Secret:    strings.ReplaceAll(s[start+m[4]:start+m[5]], "_", "-"),
			Start:     start,
			End:       end,
		})
		pos = end
	}
	return refs
}
