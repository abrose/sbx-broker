package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/abrose/sbx-broker/internal/admin"
	"github.com/abrose/sbx-broker/internal/eventlog"
	"github.com/abrose/sbx-broker/internal/secrets"
)

const gap = "  " // between two columns

var (
	bold   = lipgloss.NewStyle().Bold(true)
	active = lipgloss.NewStyle().Bold(true).Reverse(true)
)

// p makes text from the broker or a file safe to print: an error line of a
// tool or a command may hold a control sequence.
var p = eventlog.Printable

// The lines render adds around the body of a view: a blank line under the
// header and one above the footer, and the footer (the help, one line).
const (
	renderBlanks = 2
	footerLines  = 1
)

// render is the whole screen: header, the current view, footer.
func (m *Model) render() string {
	body := "asking the broker…"
	if m.loaded {
		body = m.viewBody()
	}
	if m.removal != nil {
		body += "\n\n" + m.removalLine()
	}
	return m.header() + "\n\n" + body + "\n\n" + m.help.ShortHelpView(m.footerKeys())
}

func (m *Model) viewBody() string {
	if m.form != nil {
		return m.formView()
	}
	switch m.view {
	case viewNamespaces:
		return m.namespacesView()
	case viewSecrets:
		return m.secretsView()
	case viewInspect:
		return m.inspectView()
	case viewRoutes:
		return m.routesView()
	default:
		return m.logBody()
	}
}

func (m *Model) header() string {
	parts := []string{bold.Render("sbx-broker")}
	for _, t := range tabs {
		if t.v == m.view.tab() {
			parts = append(parts, active.Render(" "+t.label+" "))
		} else {
			parts = append(parts, " "+t.label+" ")
		}
	}
	h := strings.Join(parts, " ")
	switch {
	case m.down != "":
		h += "\n" + m.down
	case m.statusErr != "":
		h += "\nstatus: " + m.statusErr
	}
	if m.note != "" {
		h += "\n" + m.note
	}
	if m.width > 0 {
		// Clip each line at the width, as the renderer clips a longer line:
		// the header reads the same, and the screen text is what shows.
		clip := lipgloss.NewStyle().MaxWidth(m.width)
		lines := strings.Split(h, "\n")
		for i, l := range lines {
			lines[i] = clip.Render(l)
		}
		h = strings.Join(lines, "\n")
	}
	return h
}

func (m *Model) namespacesView() string {
	if len(m.nss) == 0 {
		return "no namespaces"
	}
	rows := make([][]string, len(m.nss))
	for i, ns := range m.nss {
		listeners := "-"
		if len(ns.Listeners) > 0 {
			listeners = p(strings.Join(ns.Listeners, ", "))
		}
		rows[i] = []string{p(ns.Name), listeners, fmt.Sprintf("%d of %d ok", okCount(ns), len(ns.Secrets)), p(ns.Error)}
	}
	return table([]string{"NAME", "LISTENERS", "SECRETS", "ERROR"}, rows, m.nsCursor)
}

func okCount(ns admin.NamespaceStatus) int {
	n := 0
	for _, s := range ns.Secrets {
		if s.State == secrets.StateOK {
			n++
		}
	}
	return n
}

func (m *Model) secretsView() string {
	ns := m.current()
	switch {
	case ns == nil && m.ns == "":
		return "no namespace (pick one in 1 Namespaces)"
	case ns == nil:
		return "Secrets of " + p(m.ns) + "\n\nthe broker has no namespace " + p(m.ns)
	case len(ns.Secrets) == 0:
		return "Secrets of " + p(ns.Name) + "\n\nno secrets"
	}
	head := []string{"NAME", "PLACEHOLDER", "COMMAND", "STATE", "FETCHED", "SWAPS"}
	const cmdCol = 2
	rows := make([][]string, len(ns.Secrets))
	for i, s := range ns.Secrets {
		rows[i] = []string{p(s.Name), p(s.Placeholder), p(JoinArgs(s.Command)), m.state(ns.Name, s),
			fetchedAt(s), strconv.Itoa(s.Swaps)}
	}
	if m.width > 0 {
		// The command takes the room the other columns leave.
		room := m.width - len("> ") - len(gap)*(len(head)-1)
		for c, w := range widths(head, rows) {
			if c != cmdCol {
				room -= w
			}
		}
		room = max(room, len(head[cmdCol]))
		for _, r := range rows {
			r[cmdCol] = cut(r[cmdCol], room)
		}
	}
	return "Secrets of " + p(ns.Name) + "\n\n" + table(head, rows, m.secCursor)
}

func (m *Model) state(ns string, s secrets.Status) string {
	if m.fetching[ns+"/"+s.Name] > 0 || m.fetching[ns+"/"] > 0 {
		return "fetching"
	}
	return p(string(s.State))
}

func fetchedAt(s secrets.Status) string {
	if s.FetchedAt.IsZero() {
		return "-"
	}
	return s.FetchedAt.Local().Format(time.TimeOnly)
}

func (m *Model) inspectView() string {
	ns, s := m.current(), m.inspected()
	if s == nil {
		return "the broker has no secret " + p(m.secret) + " in namespace " + p(m.ns)
	}
	fields := [][]string{
		{"name", p(s.Name)},
		{"placeholder", p(s.Placeholder)},
		{"command", p(JoinArgs(s.Command))},
		{"state", m.state(ns.Name, *s)},
		{"fetched", fetchedAt(*s)},
	}
	if s.State == secrets.StateFailed {
		fields = append(fields, []string{"exit code", strconv.Itoa(s.ExitCode)}, []string{"error", p(s.Err)})
	}
	length, sum := "-", "-"
	if s.Fingerprint != "" {
		length, sum = strconv.Itoa(s.Length), "sha256 "+p(s.Fingerprint)
	}
	fields = append(fields, []string{"length", length}, []string{"fingerprint", sum},
		[]string{"swaps", strconv.Itoa(s.Swaps)})
	return "Secret " + p(s.Name) + " of " + p(ns.Name) + "\n\n" + table(nil, fields, -1) + "\n\n" + m.valueLine(s.Name)
}

// table lays out rows in columns, under head when it is not nil. The row at
// cursor gets a "> " mark; -1 marks none.
func table(head []string, rows [][]string, cursor int) string {
	w := widths(head, rows)
	line := func(cells []string, mark string) string {
		var b strings.Builder
		b.WriteString(mark)
		for i, c := range cells {
			if i > 0 {
				b.WriteString(gap)
			}
			b.WriteString(c)
			if i < len(cells)-1 {
				b.WriteString(strings.Repeat(" ", w[i]-lipgloss.Width(c)))
			}
		}
		return strings.TrimRight(b.String(), " ")
	}
	var lines []string
	indent := ""
	if cursor >= 0 {
		indent = "  "
	}
	if head != nil {
		lines = append(lines, bold.Render(line(head, indent)))
	}
	for i, r := range rows {
		mark := indent
		if i == cursor {
			mark = "> "
		}
		lines = append(lines, line(r, mark))
	}
	return strings.Join(lines, "\n")
}

func widths(head []string, rows [][]string) []int {
	var w []int
	for _, r := range append([][]string{head}, rows...) {
		for i, c := range r {
			if i >= len(w) {
				w = append(w, 0)
			}
			w[i] = max(w[i], lipgloss.Width(c))
		}
	}
	return w
}

// cut shortens s to width cells, the last one an ellipsis.
func cut(s string, width int) string {
	if lipgloss.Width(s) <= width {
		return s
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if used+rw > width-1 {
			break
		}
		b.WriteRune(r)
		used += rw
	}
	return b.String() + "…"
}
