package tui

import (
	"errors"
	"regexp"
	"strings"
)

// SplitArgs splits a command line into its argv, as a shell splits words,
// but with no expansion and no shell (designer's call C4): blanks (space,
// tab, newline) split; single quotes keep everything up to the next single
// quote; double quotes group, and in them a backslash escapes only \, ",
// $ and `; outside quotes a backslash escapes the next character.
func SplitArgs(s string) ([]string, error) {
	var (
		argv  []string
		word  strings.Builder
		in    bool // a word has started, maybe as an empty quoted one
		quote rune // ' or " while in quotes
		esc   bool // the last character was a backslash
	)
	for _, r := range s {
		switch {
		case esc && quote == '"' && !strings.ContainsRune("\\\"$`", r):
			word.WriteRune('\\') // in double quotes, a backslash before another character stays
			word.WriteRune(r)
			esc = false
		case esc:
			word.WriteRune(r)
			esc = false
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				word.WriteRune(r)
			}
		case quote == '"':
			switch r {
			case '\\':
				esc = true
			case '"':
				quote = 0
			default:
				word.WriteRune(r)
			}
		case r == ' ' || r == '\t' || r == '\n':
			if in {
				argv = append(argv, word.String())
				word.Reset()
				in = false
			}
		case r == '\\':
			esc, in = true, true
		case r == '\'' || r == '"':
			quote, in = r, true
		default:
			word.WriteRune(r)
			in = true
		}
	}
	switch {
	case quote != 0:
		return nil, errors.New("unclosed quote")
	case esc:
		return nil, errors.New("trailing backslash")
	}
	if in {
		argv = append(argv, word.String())
	}
	if len(argv) == 0 {
		return nil, errors.New("empty command")
	}
	return argv, nil
}

var plainArg = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// JoinArgs writes argv as one line that SplitArgs reads back as argv. An
// argument of only safe characters stays as it is; any other is put in
// single quotes, and a single quote in it ends the quotes, comes as \' and
// opens them again.
func JoinArgs(argv []string) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if plainArg.MatchString(a) {
			out[i] = a
		} else {
			out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
	}
	return strings.Join(out, " ")
}
