package eventlog

import (
	"strings"
	"unicode/utf8"
)

const hexDigits = "0123456789abcdef"

// Printable returns s safe to print on a terminal. Event fields such as the
// path, the method and the message come from a sandboxed app; printed raw,
// an escape sequence in them would run on the owner's terminal, and a
// newline would forge a log line. Printable writes every C0 control (below
// 0x20), DEL (0x7f), every C1 control (U+0080 to U+009F) and every byte
// that is not valid UTF-8 as \xNN, and a backslash as \\, so the output is
// unambiguous. Every printer of events (the CLI, the TUI) must use it.
func Printable(s string) string {
	if isPrintable(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size <= 1:
			writeHex(&b, s[i])
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			writeHex(&b, byte(r))
		case r == '\\':
			b.WriteString(`\\`)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// PrintableField is Printable with each space written as %20, for a field
// in a line of space-separated fields (such as a path or a method).
func PrintableField(s string) string {
	return strings.ReplaceAll(Printable(s), " ", "%20")
}

func isPrintable(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c >= 0x7f || c == '\\' {
			return false // non-ASCII takes the slow path, which keeps valid runes
		}
	}
	return true
}

func writeHex(b *strings.Builder, c byte) {
	b.WriteString(`\x`)
	b.WriteByte(hexDigits[c>>4])
	b.WriteByte(hexDigits[c&0xf])
}
