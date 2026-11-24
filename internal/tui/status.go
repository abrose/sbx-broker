package tui

import (
	"errors"

	tea "charm.land/bubbletea/v2"

	"github.com/abrose/sbx-broker/internal/admin"
	"github.com/abrose/sbx-broker/internal/eventlog"
	"github.com/abrose/sbx-broker/internal/secrets"
)

// Messages. Every client call runs in a tea.Cmd and answers with one.
type (
	pollMsg   struct{}
	statusMsg struct {
		nss       []admin.NamespaceStatus
		err       error
		once      bool // an extra status after an edit: the poll chain goes on as it is
		refreshes int  // Model.refreshes when it was sent
	}
	refreshMsg struct {
		key string // ns + "/" + secret, as in Model.fetching
		nss []admin.NamespaceStatus
		err error
	}
)

func (m *Model) statusCmd() tea.Cmd {
	cl, refreshes := m.cl, m.refreshes
	return func() tea.Msg {
		nss, err := cl.Status("")
		return statusMsg{nss: nss, err: err, refreshes: refreshes}
	}
}

func (m *Model) gotStatus(msg statusMsg) {
	if msg.refreshes < m.refreshes {
		return // older than a refresh's answer; the next poll brings the news
	}
	m.down, m.statusErr = "", ""
	switch {
	case errors.Is(msg.err, admin.ErrNoBroker):
		m.down = NoBroker // keep the last status on screen; the next poll tries again
	case errors.Is(msg.err, admin.ErrNoAnswer):
		m.down = admin.ErrNoAnswer.Error() // the same: the broker may be stopped (ctrl+z) or hang
	case msg.err != nil:
		m.statusErr = eventlog.Printable(msg.err.Error())
	default:
		m.nss, m.loaded = msg.nss, true
		m.nsCursor = min(m.nsCursor, max(len(m.nss)-1, 0))
		if ns := m.current(); ns != nil {
			m.secCursor = min(m.secCursor, max(len(ns.Secrets)-1, 0))
			m.routeCursor = min(m.routeCursor, max(routeRows(ns)-1, 0))
		}
		if m.inspected() == nil {
			m.hide() // the secret is gone, so Inspect cannot show its value
			m.confirm = false
		}
	}
}

// inspected returns the status of the secret of Inspect, or nil when the
// broker has no such secret.
func (m *Model) inspected() *secrets.Status {
	ns := m.current()
	if ns == nil {
		return nil
	}
	for i := range ns.Secrets {
		if ns.Secrets[i].Name == m.secret {
			return &ns.Secrets[i]
		}
	}
	return nil
}

func (m *Model) refreshCmd(ns, secret string) tea.Cmd {
	k := ns + "/" + secret
	m.fetching[k]++
	cl := m.cl
	return func() tea.Msg {
		nss, err := cl.Refresh(ns, secret)
		return refreshMsg{key: k, nss: nss, err: err}
	}
}

func (m *Model) gotRefresh(msg refreshMsg) {
	if m.fetching[msg.key]--; m.fetching[msg.key] <= 0 {
		delete(m.fetching, msg.key)
	}
	m.refreshes++
	switch {
	case errors.Is(msg.err, admin.ErrNoBroker):
		m.down = NoBroker
	case msg.err != nil:
		m.note = "refresh " + eventlog.Printable(msg.key) + ": " + eventlog.Printable(msg.err.Error())
	default:
		for _, n := range msg.nss {
			for i := range m.nss {
				if m.nss[i].Name == n.Name {
					m.nss[i] = n
				}
			}
		}
	}
}

// statusOnce asks for a status now, outside the 2 s poll chain.
func (m *Model) statusOnce() tea.Cmd {
	cl, refreshes := m.cl, m.refreshes
	return func() tea.Msg {
		nss, err := cl.Status("")
		return statusMsg{nss: nss, err: err, once: true, refreshes: refreshes}
	}
}
