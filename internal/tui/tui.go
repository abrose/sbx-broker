// Package tui is the terminal UI of sbx-broker: the namespaces, their
// secrets, a secret's details and a confirmed reveal of its value. It shows
// what the broker loaded (designer's call C1) and talks to the broker only
// through a Client.
//
// The TUI never holds a value it does not show: a revealed value is wiped
// (every byte set to 0) and dropped at the next key press, at a view switch,
// 30 s after it appeared, and when the TUI quits. It runs in the alternate
// screen, so no value stays in the terminal's scrollback.
package tui

import (
	"errors"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/abrose/sbx-broker/internal/admin"
	"github.com/abrose/sbx-broker/internal/eventlog"
	"github.com/abrose/sbx-broker/internal/secrets"
)

// NoBroker is the line the TUI and every command show when no broker
// answers on the admin socket.
const NoBroker = "no broker runs (start it with: sbx-broker serve, or as a service with: sbx-broker install)"

const (
	pollEvery = 2 * time.Second  // designer's call C6
	revealFor = 30 * time.Second // a shown value hides after this
)

// Options configure the TUI.
type Options struct {
	ConfigDir string // the namespace files
	Namespace string // the namespace of the project marker, if any: the TUI starts on its Secrets (C7)
	// Editor runs the owner's editor in dir with argv. The edit forms use it.
	Editor func(dir string, argv []string) error
}

type view int

const (
	viewNamespaces view = iota
	viewSecrets
	viewRoutes
	viewLog
	viewInspect // a part of Secrets: tab and the tabs treat it as Secrets
)

var tabs = []struct {
	v     view
	label string
}{{viewNamespaces, "1 Namespaces"}, {viewSecrets, "2 Secrets"}, {viewRoutes, "3 Routes"}, {viewLog, "4 Log"}}

func (v view) tab() view {
	if v == viewInspect {
		return viewSecrets
	}
	return v
}

// Messages. Every client call runs in a tea.Cmd and answers with one.
type (
	pollMsg   struct{}
	statusMsg struct {
		nss []admin.NamespaceStatus
		err error
	}
	refreshMsg struct {
		key string // ns + "/" + secret, as in Model.fetching
		nss []admin.NamespaceStatus
		err error
	}
	revealMsg struct {
		id    int
		value []byte
		err   error
	}
	hideMsg struct{ id int } // the 30 s timer of the reveal id
)

// Model is the TUI's state. Bubble Tea calls Update and View from one
// goroutine; the commands it runs elsewhere touch only the Client.
type Model struct {
	cl    Client
	opt   Options
	keys  keyMap
	help  help.Model
	width int // 0 until the first tea.WindowSizeMsg: no command is cut
	// after returns a command that sends msg after d; tests replace it.
	after func(d time.Duration, msg tea.Msg) tea.Cmd

	view      view
	nss       []admin.NamespaceStatus
	loaded    bool   // a status came
	gone      bool   // the last status found no broker
	statusErr string // the last status error other than no broker, printable
	note      string // the outcome of the last action, printable; cleared at the next key

	nsCursor  int
	ns        string // the namespace of Secrets and Inspect; "" until one is picked
	secCursor int
	secret    string          // the secret of Inspect
	fetching  map[string]bool // ns + "/" + secret (secret "" for all) of each refresh in flight

	confirm   bool   // r was pressed in Inspect; the next key decides
	reveals   int    // the id of the last reveal asked for
	pending   int    // the id of the reveal whose answer is awaited; 0: none
	shown     int    // the id of the shown value; 0: none
	value     []byte // the shown value, wiped when it hides
	revealErr string // why the last reveal failed, printable
}

// New returns the model for cl. With opt.Namespace it starts on the Secrets
// of that namespace, else on Namespaces.
func New(cl Client, opt Options) *Model {
	m := &Model{
		cl:       cl,
		opt:      opt,
		keys:     newKeyMap(),
		help:     help.New(),
		after:    func(d time.Duration, msg tea.Msg) tea.Cmd { return tea.Tick(d, func(time.Time) tea.Msg { return msg }) },
		fetching: map[string]bool{},
	}
	if opt.Namespace != "" {
		m.ns, m.view = opt.Namespace, viewSecrets
	}
	return m
}

// Run shows the TUI in the alternate screen until the owner quits.
func Run(cl Client, opt Options) error {
	m := New(cl, opt)
	_, err := tea.NewProgram(m).Run()
	m.hide() // the model hid it on quit already; this covers a crash of the program
	return err
}

// Init asks for the first status.
func (m *Model) Init() tea.Cmd { return m.statusCmd() }

// View renders the model in the alternate screen.
func (m *Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	return v
}

// Update handles one message.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.help.SetWidth(msg.Width)
	case tea.KeyPressMsg:
		return m, m.key(msg)
	case tea.InterruptMsg:
		m.hide()
		return m, tea.Quit
	case pollMsg:
		return m, m.statusCmd()
	case statusMsg:
		m.gotStatus(msg)
		return m, m.after(pollEvery, pollMsg{})
	case refreshMsg:
		m.gotRefresh(msg)
	case revealMsg:
		return m, m.gotReveal(msg)
	case hideMsg:
		if msg.id == m.shown {
			m.hide()
		}
	}
	return m, nil
}

func (m *Model) key(k tea.KeyPressMsg) tea.Cmd {
	if key.Matches(k, m.keys.forceQuit) {
		m.hide()
		return tea.Quit
	}
	if m.confirm {
		m.confirm = false
		if key.Matches(k, m.keys.confirm) {
			return m.revealCmd()
		}
		return nil // any other key cancels
	}
	// Any key hides a shown value (and a reveal error) and drops a reveal
	// whose answer has not come yet.
	m.hide()
	m.note = ""
	switch {
	case key.Matches(k, m.keys.quit):
		return tea.Quit
	case key.Matches(k, m.keys.next):
		m.view = tabs[(int(m.view.tab())+1)%len(tabs)].v
		return nil
	}
	for i, b := range m.keys.views {
		if key.Matches(k, b) {
			m.view = tabs[i].v
			return nil
		}
	}
	switch m.view {
	case viewNamespaces:
		m.namespacesKey(k)
	case viewSecrets:
		return m.secretsKey(k)
	case viewInspect:
		m.inspectKey(k)
	}
	return nil
}

func (m *Model) namespacesKey(k tea.KeyPressMsg) {
	switch {
	case key.Matches(k, m.keys.up):
		m.nsCursor = max(m.nsCursor-1, 0)
	case key.Matches(k, m.keys.down):
		m.nsCursor = min(m.nsCursor+1, max(len(m.nss)-1, 0))
	case key.Matches(k, m.keys.open):
		if m.nsCursor < len(m.nss) {
			m.ns, m.secCursor, m.view = m.nss[m.nsCursor].Name, 0, viewSecrets
		}
	}
}

func (m *Model) secretsKey(k tea.KeyPressMsg) tea.Cmd {
	ns := m.current()
	if ns == nil {
		return nil
	}
	switch {
	case key.Matches(k, m.keys.up):
		m.secCursor = max(m.secCursor-1, 0)
	case key.Matches(k, m.keys.down):
		m.secCursor = min(m.secCursor+1, max(len(ns.Secrets)-1, 0))
	case key.Matches(k, m.keys.inspect):
		if m.secCursor < len(ns.Secrets) {
			m.ns, m.secret, m.view = ns.Name, ns.Secrets[m.secCursor].Name, viewInspect
		}
	case key.Matches(k, m.keys.refresh):
		if m.secCursor < len(ns.Secrets) {
			return m.refreshCmd(ns.Name, ns.Secrets[m.secCursor].Name)
		}
	case key.Matches(k, m.keys.refreshAll):
		return m.refreshCmd(ns.Name, "")
	}
	return nil
}

func (m *Model) inspectKey(k tea.KeyPressMsg) {
	switch {
	case key.Matches(k, m.keys.reveal):
		m.confirm = true
	case key.Matches(k, m.keys.back):
		m.view = viewSecrets
	}
}

// current returns the namespace of Secrets: the picked one, else the one
// under the cursor of Namespaces. It is nil when the broker has none such.
func (m *Model) current() *admin.NamespaceStatus {
	name := m.ns
	if name == "" && m.nsCursor < len(m.nss) {
		name = m.nss[m.nsCursor].Name
	}
	for i := range m.nss {
		if m.nss[i].Name == name {
			return &m.nss[i]
		}
	}
	return nil
}

func (m *Model) statusCmd() tea.Cmd {
	cl := m.cl
	return func() tea.Msg {
		nss, err := cl.Status("")
		return statusMsg{nss: nss, err: err}
	}
}

func (m *Model) gotStatus(msg statusMsg) {
	m.gone, m.statusErr = false, ""
	switch {
	case errors.Is(msg.err, admin.ErrNoBroker):
		m.gone = true // keep the last status on screen; the next poll tries again
	case msg.err != nil:
		m.statusErr = eventlog.Printable(msg.err.Error())
	default:
		m.nss, m.loaded = msg.nss, true
		m.nsCursor = min(m.nsCursor, max(len(m.nss)-1, 0))
		if ns := m.current(); ns != nil {
			m.secCursor = min(m.secCursor, max(len(ns.Secrets)-1, 0))
		}
		if m.inspected() == nil {
			m.hide() // the secret is gone, so Inspect cannot show its value
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
	m.fetching[k] = true
	cl := m.cl
	return func() tea.Msg {
		nss, err := cl.Refresh(ns, secret)
		return refreshMsg{key: k, nss: nss, err: err}
	}
}

func (m *Model) gotRefresh(msg refreshMsg) {
	delete(m.fetching, msg.key)
	switch {
	case errors.Is(msg.err, admin.ErrNoBroker):
		m.gone = true
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

type keyMap struct {
	views               []key.Binding // 1 to 4, in the order of tabs
	next, quit          key.Binding
	forceQuit           key.Binding
	up, down            key.Binding
	open, inspect       key.Binding
	refresh, refreshAll key.Binding
	reveal, confirm     key.Binding
	back, viewsHelp     key.Binding
}

func newKeyMap() keyMap {
	b := func(keys []string, k, desc string) key.Binding {
		return key.NewBinding(key.WithKeys(keys...), key.WithHelp(k, desc))
	}
	return keyMap{
		views: []key.Binding{
			b([]string{"1"}, "1", "namespaces"), b([]string{"2"}, "2", "secrets"),
			b([]string{"3"}, "3", "routes"), b([]string{"4"}, "4", "log"),
		},
		next: b([]string{"tab"}, "tab", "next view"),
		// q is not a quit while a text field has focus (the edit forms).
		quit:       b([]string{"q"}, "q", "quit"),
		forceQuit:  b([]string{"ctrl+c"}, "ctrl+c", "quit"),
		up:         b([]string{"up", "k"}, "↑/k", "up"),
		down:       b([]string{"down", "j"}, "↓/j", "down"),
		open:       b([]string{"enter"}, "enter", "secrets"),
		inspect:    b([]string{"enter"}, "enter", "inspect"),
		refresh:    b([]string{"f"}, "f", "refresh"),
		refreshAll: b([]string{"F"}, "F", "refresh all"),
		reveal:     b([]string{"r"}, "r", "reveal"),
		confirm:    b([]string{"y"}, "y", "yes"),
		back:       b([]string{"esc"}, "esc", "back"),
		viewsHelp:  b([]string{"1", "2", "3", "4", "tab"}, "1-4/tab", "views"), // for the footer only
	}
}

// footerKeys are the keys of the current view.
func (m *Model) footerKeys() []key.Binding {
	k := m.keys
	var own []key.Binding
	switch m.view {
	case viewNamespaces:
		own = []key.Binding{k.up, k.down, k.open}
	case viewSecrets:
		own = []key.Binding{k.up, k.down, k.inspect, k.refresh, k.refreshAll}
	case viewInspect:
		own = []key.Binding{k.reveal, k.back}
	}
	return append(own, k.viewsHelp, k.quit)
}
