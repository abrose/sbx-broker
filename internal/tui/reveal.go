package tui

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/abrose/sbx-broker/internal/eventlog"
)

type (
	revealMsg struct {
		id    int
		value []byte
		err   error
	}
	hideMsg struct{ id int } // the 30 s timer of the reveal id
)

func (m *Model) inspectKey(k tea.KeyPressMsg) {
	switch {
	case key.Matches(k, m.keys.reveal):
		m.confirm = true
	case key.Matches(k, m.keys.back):
		m.view = viewSecrets
	}
}

func (m *Model) revealCmd() tea.Cmd {
	m.reveals++
	id, ns, secret, cl := m.reveals, m.ns, m.secret, m.cl
	m.pending = id
	return func() tea.Msg {
		v, err := cl.Reveal(ns, secret)
		return revealMsg{id: id, value: v, err: err}
	}
}

func (m *Model) gotReveal(msg revealMsg) tea.Cmd {
	if msg.id != m.pending || m.view != viewInspect {
		clear(msg.value) // the owner moved on before the answer came
		return nil
	}
	m.pending = 0
	if msg.err != nil {
		clear(msg.value)
		m.revealErr = eventlog.Printable(msg.err.Error())
		return nil
	}
	m.value, m.shown = msg.value, msg.id
	return m.after(revealFor, hideMsg{id: msg.id})
}

// hide wipes and drops the shown value, clears a reveal error and forgets
// a reveal whose answer has not come yet.
func (m *Model) hide() {
	clear(m.value)
	m.value, m.shown, m.pending, m.revealErr = nil, 0, 0, ""
}

func (m *Model) valueLine(name string) string {
	switch {
	case m.confirm:
		return "Reveal the value of " + p(name) + "? The broker logs it. (y/n)"
	case m.value != nil:
		v := string(m.value)
		line := "value: " + p(v)
		if p(v) != v {
			line += "   (escaped)" // the screen does not show the value as it is
		}
		return line + "   (any key hides it)"
	case m.revealErr != "":
		return "reveal failed: " + m.revealErr
	case m.pending != 0:
		return "value: asking the broker…"
	}
	return "value: hidden (r to reveal)"
}
