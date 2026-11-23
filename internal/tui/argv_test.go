package tui

import (
	"slices"
	"testing"
)

var splitCases = []struct {
	in   string
	want []string
}{
	{`gopass show -o <path>`, []string{"gopass", "show", "-o", "<path>"}},
	{`sh -c "grep '^<host>:5432:' ~/.pgpass | cut -d: -f5"`, []string{"sh", "-c", "grep '^<host>:5432:' ~/.pgpass | cut -d: -f5"}},
	{`a\ b 'c d' "e\"f"`, []string{"a b", "c d", `e"f`}},
	{`a '' "" b`, []string{"a", "", "", "b"}},
	{"  a\tb  ", []string{"a", "b"}},
	{`'it'\''s' "a\b" "\\$\` + "`" + `"`, []string{"it's", `a\b`, `\$` + "`"}},
	{`op read op://<vault>/<item>/credential`, []string{"op", "read", "op://<vault>/<item>/credential"}},
}

func TestSplitArgs(t *testing.T) {
	for _, c := range splitCases {
		got, err := SplitArgs(c.in)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("SplitArgs(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	for in, want := range map[string]string{
		`"open`:   "unclosed quote",
		`'open`:   "unclosed quote",
		``:        "empty command",
		`   `:     "empty command",
		`a b\`:    "trailing backslash",
		`a "b\"`:  "unclosed quote",
		"\t \n  ": "empty command",
	} {
		got, err := SplitArgs(in)
		if err == nil || err.Error() != want {
			t.Errorf("SplitArgs(%q) = %q, %v; want error %q", in, got, err, want)
		}
	}
}

func TestJoinArgsRoundTrip(t *testing.T) {
	extra := [][]string{
		{"a", "", "b"},
		{"it's", `back\slash`, "$HOME", "semi;colon", "new\nline", "tab\there", "ümlaut"},
		{"plain_@%+=:,./-arg"},
	}
	for _, r := range append(splitResults(), extra...) {
		joined := JoinArgs(r)
		got, err := SplitArgs(joined)
		if err != nil || !slices.Equal(got, r) {
			t.Errorf("SplitArgs(JoinArgs(%q)) = SplitArgs(%q) = %q, %v", r, joined, got, err)
		}
	}
}

func TestJoinArgsQuotesOnlyWhenNeeded(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want string
	}{
		{[]string{"gopass", "show", "-o", "a/b"}, "gopass show -o a/b"},
		{[]string{"op", "read", "op://<vault>/<item>/credential"}, "op read 'op://<vault>/<item>/credential'"},
		{[]string{"echo", "it's", ""}, `echo 'it'\''s' ''`},
	} {
		if got := JoinArgs(c.in); got != c.want {
			t.Errorf("JoinArgs(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func splitResults() [][]string {
	var out [][]string
	for _, c := range splitCases {
		out = append(out, c.want)
	}
	return out
}
