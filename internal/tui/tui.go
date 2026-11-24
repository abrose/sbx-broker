// Package tui is the terminal UI of sbx-broker: the namespaces, their
// secrets, routes and databases, a secret's details, a confirmed reveal of its
// value, the live log, and forms that edit the namespace files. It shows what
// the broker loaded (designer's call C1) and talks to the broker only through
// a Client.
//
// The TUI never holds a value it does not show: a revealed value is wiped
// (every byte set to 0) and dropped at the next key press, at a view switch,
// 30 s after it appeared, and when the TUI quits. It runs in the alternate
// screen, so no value stays in the terminal's scrollback. The value's []byte
// is wiped; the rendered screen text is a Go string that the TUI cannot wipe,
// and it is dropped with the next frame.
package tui

import (
	"errors"
	"os"
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
	// Editor runs the owner's editor in dir with argv on term, the terminal
	// the TUI hands over. The key o uses it.
	Editor func(dir string, argv []string, term Terminal) error
	Getenv func(string) string // reads $VISUAL and $EDITOR; nil: os.Getenv
	Note   string              // a line the TUI shows as its note at start, printable: the caller escapes it
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

// Model is the TUI's state. Bubble Tea calls Update and View from one
// goroutine; the commands it runs elsewhere touch only the Client.
type Model struct {
	cl     Client
	opt    Options
	keys   keyMap
	help   help.Model
	width  int // 0 until the first tea.WindowSizeMsg: no command is cut
	height int
	log    logView
	// after returns a command that sends msg after d; tests replace it.
	after func(d time.Duration, msg tea.Msg) tea.Cmd
	// exec gives the terminal to c (tea.Exec); tests replace it.
	exec func(c tea.ExecCommand, fn tea.ExecCallback) tea.Cmd

	view      view
	nss       []admin.NamespaceStatus
	loaded    bool   // a status came
	down      string // why the last status got no answer: NoBroker or ErrNoAnswer's text; "": it answered
	statusErr string // the last status error other than no broker, printable
	note      string // the outcome of the last action, printable; cleared at the next key

	nsCursor  int
	ns        string // the namespace of Secrets and Inspect; "" until one is picked
	secCursor int
	secret    string         // the secret of Inspect
	fetching  map[string]int // the refreshes in flight per ns + "/" + secret (secret "" for all)
	refreshes int            // the refresh answers taken; a status sent before the last one is stale

	confirm   bool   // r was pressed in Inspect; the next key decides
	reveals   int    // the id of the last reveal asked for
	pending   int    // the id of the reveal whose answer is awaited; 0: none
	shown     int    // the id of the shown value; 0: none
	value     []byte // the shown value, wiped when it hides
	revealErr string // why the last reveal failed, printable

	routeCursor int      // the row of Routes: the HTTP routes, then the databases
	form        *form    // the open add or edit form; nil: none
	removal     *removal // the remove that waits for its y; nil: none
	edits       int      // the id of the last form or removal
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
		exec:     tea.Exec,
		fetching: map[string]int{},
		note:     opt.Note,
	}
	if opt.Namespace != "" {
		m.ns, m.view = opt.Namespace, viewSecrets
	}
	return m
}

// Run shows the TUI in the alternate screen until the owner quits. It clears
// TEA_TRACE, TEA_DEBUG and UV_DEBUG first (see debugLogs).
func Run(cl Client, opt Options) error { return run(cl, opt) }

// debugLogs are the variables that make the terminal libraries write a file,
// cleared before the program starts. Re-check them at every bump of
// bubbletea or ultraviolet. In bubbletea v2.1.2 and ultraviolet
// v0.0.0-20261008173134-6b8d4baf91b4:
//   - TEA_TRACE (tea.go:664, 1059-1062; cursed_renderer.go:272-273, 645-646)
//     appends every frame, a shown value included, to the file it names;
//   - TEA_DEBUG (tea.go:1407-1415, 1432-1440) writes bubbletea-panic-<unix>.log
//     with the panic and its stack into the current folder, often a project
//     folder the sandbox reads;
//   - UV_DEBUG (ultraviolet terminal_screen.go:70-74) logs the renderer's
//     output to the file it names; bubbletea v2.1.2 does not reach it.
var debugLogs = []string{"TEA_TRACE", "TEA_DEBUG", "UV_DEBUG"}

// run is Run with options for the program; tests run it without a terminal.
func run(cl Client, opt Options, popts ...tea.ProgramOption) error {
	for _, k := range debugLogs {
		if err := os.Unsetenv(k); err != nil {
			return err
		}
	}
	m := New(cl, opt)
	_, err := tea.NewProgram(m, popts...).Run()
	m.hide() // the wipe for SIGINT and SIGTERM, which end Run without Update, and for a crash
	m.stopFollow()
	return quitErr(err)
}

// quitErr is the error of Run: a SIGINT is the owner's quit, not a failure.
func quitErr(err error) error {
	if errors.Is(err, tea.ErrInterrupted) {
		return nil
	}
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
		m.width, m.height = msg.Width, msg.Height
		m.help.SetWidth(msg.Width)
	case tea.KeyPressMsg:
		return m, m.key(msg)
	case tea.PasteMsg:
		if m.form != nil {
			return m, m.formInput(msg)
		}
	case pollMsg:
		return m, m.statusCmd()
	case statusMsg:
		m.gotStatus(msg)
		var cmds []tea.Cmd
		if !msg.once {
			cmds = append(cmds, m.after(pollEvery, pollMsg{}))
		}
		if m.down == "" && m.statusErr == "" {
			cmds = append(cmds, m.followCmd()) // the broker answers: follow its log if no follow runs
		}
		return m, tea.Batch(cmds...)
	case followMsg:
		return m, m.after(logEvery, logTickMsg{})
	case logTickMsg:
		return m, m.logTick()
	case refreshMsg:
		m.gotRefresh(msg)
	case revealMsg:
		return m, m.gotReveal(msg)
	case hideMsg:
		if msg.id == m.shown {
			m.hide()
		}
	case formBaseMsg:
		m.gotBase(msg)
	case savedMsg:
		return m, m.gotSaved(msg)
	case editedMsg:
		return m, m.gotEdited(msg)
	case reloadedMsg:
		m.gotReloaded(msg)
	}
	return m, nil
}

func (m *Model) key(k tea.KeyPressMsg) tea.Cmd {
	if key.Matches(k, m.keys.forceQuit) {
		return m.quit()
	}
	if m.form != nil {
		return m.formKey(k) // a text field takes q and the digits as text
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
	if m.removal != nil {
		return m.removalKey(k)
	}
	switch {
	case key.Matches(k, m.keys.quit):
		return m.quit()
	case key.Matches(k, m.keys.next):
		m.view = tabs[(int(m.view.tab())+1)%len(tabs)].v
		return nil
	case key.Matches(k, m.keys.prev):
		m.view = tabs[(int(m.view.tab())+len(tabs)-1)%len(tabs)].v
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
	case viewRoutes:
		return m.routesKey(k)
	case viewInspect:
		m.inspectKey(k)
	case viewLog:
		m.logKey(k)
	}
	return nil
}

// quit hides a shown value, cancels the log follow and ends the program.
func (m *Model) quit() tea.Cmd {
	m.hide()
	m.stopFollow()
	return tea.Quit
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
	if key.Matches(k, m.keys.editor) {
		return m.editorCmd(m.nsName()) // also for a namespace the broker has not loaded
	}
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
	default:
		return m.secretsEditKey(k, ns)
	}
	return nil
}

// nsName is the name of the namespace of Secrets: the picked one, else the
// one under the cursor of Namespaces; "" for none.
func (m *Model) nsName() string {
	if m.ns == "" && m.nsCursor < len(m.nss) {
		return m.nss[m.nsCursor].Name
	}
	return m.ns
}

// current returns the status of the namespace of Secrets, or nil when the
// broker has none such.
func (m *Model) current() *admin.NamespaceStatus { return m.status(m.nsName()) }

// status returns the status of the namespace name, or nil.
func (m *Model) status(name string) *admin.NamespaceStatus {
	for i := range m.nss {
		if m.nss[i].Name == name {
			return &m.nss[i]
		}
	}
	return nil
}

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
