package tui

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/abrose/sbx-broker/internal/admin"
	"github.com/abrose/sbx-broker/internal/config"
	"github.com/abrose/sbx-broker/internal/nsfile"
)

// The edit forms write the namespace file themselves (designer's call C2),
// only through nsfile: it keeps the comments and the key order, refuses a file
// that changed on disk, runs config.Check and writes atomically. Then the TUI
// sends reload. A name or alias is fixed after creation, and transforms are
// not in a form: o opens the file in the owner's editor for them (C3).

// Terminal is the terminal Bubble Tea hands over while the editor runs.
type Terminal struct {
	In       io.Reader
	Out, Err io.Writer
}

// EditorArgv is the editor command for file: $VISUAL, else $EDITOR, else vi,
// split on spaces and run with no shell.
func EditorArgv(getenv func(string) string, file string) []string {
	for _, key := range []string{"VISUAL", "EDITOR"} {
		if fields := strings.Fields(getenv(key)); len(fields) > 0 {
			return append(fields, file)
		}
	}
	return []string{"vi", file}
}

// editorExec is the tea.ExecCommand of the editor key: Bubble Tea sets the
// terminal on it, and Run hands that terminal to Options.Editor.
type editorExec struct {
	run  func(dir string, argv []string, term Terminal) error
	dir  string
	argv []string
	term Terminal
}

func (e *editorExec) SetStdin(r io.Reader)  { e.term.In = r }
func (e *editorExec) SetStdout(w io.Writer) { e.term.Out = w }
func (e *editorExec) SetStderr(w io.Writer) { e.term.Err = w }
func (e *editorExec) Run() error            { return e.run(e.dir, e.argv, e.term) }

type formKind int

const (
	secretForm formKind = iota
	routeForm
	databaseForm
)

// formSpecs name each kind and list the fields of its form, in order. The
// first field is the name or alias.
var formSpecs = [...]struct {
	noun, a string // "HTTP route", "an HTTP route"
	labels  []string
}{
	secretForm:   {"secret", "a secret", []string{"name", "command"}},
	routeForm:    {"HTTP route", "an HTTP route", []string{"name", "listen", "upstream"}},
	databaseForm: {"database", "a database", []string{"alias", "upstream", "dbname", "user", "password secret", "require-auth", "postgres listen"}},
}

// form is an open add or edit form.
type form struct {
	id     int
	kind   formKind
	edit   bool // the name or alias, the first field, is fixed (C3)
	ns     string
	base   []byte // the namespace file as the form opened it: a save refuses when the file changed since
	inputs []textinput.Model
	focus  int
	err    string // why the last save failed, printable
	saving bool
}

// removal is a remove that waits for its y.
type removal struct {
	kind     formKind
	ns, name string
}

// Messages of the forms and the editor key.
type (
	formBaseMsg struct {
		id   int
		data []byte
		err  error
	}
	savedMsg struct {
		id        int    // the form or removal that saved
		done      string // what the save did, for the note: "saved secret x"
		err       error  // the save failed: nothing was written
		reloadErr error  // the save worked; the reload did not
	}
	editedMsg   struct{ err error } // the editor ended
	reloadedMsg struct{ err error } // the reload after the editor failed
)

func (m *Model) nsPath(ns string) string { return filepath.Join(m.opt.ConfigDir, ns+".yaml") }

// openForm opens a form for ns with values in its fields, and reads the
// namespace file the save will start from.
func (m *Model) openForm(kind formKind, ns string, edit bool, values ...string) tea.Cmd {
	m.hide()
	m.edits++
	f := &form{id: m.edits, kind: kind, edit: edit, ns: ns}
	for i := range formSpecs[kind].labels {
		in := textinput.New()
		in.Prompt = ""
		st := in.Styles()
		st.Cursor.Blink = false // a blinking cursor would send a tick every half second
		in.SetStyles(st)
		if i < len(values) {
			in.SetValue(values[i])
		}
		f.inputs = append(f.inputs, in)
	}
	if edit {
		f.focus = 1
	}
	f.inputs[f.focus].Focus()
	m.form = f
	path, id := m.nsPath(ns), f.id
	return func() tea.Msg {
		data, err := os.ReadFile(path)
		return formBaseMsg{id: id, data: data, err: err}
	}
}

func (m *Model) gotBase(msg formBaseMsg) {
	if f := m.form; f != nil && f.id == msg.id {
		if msg.err != nil {
			f.err = p(msg.err.Error())
			return
		}
		f.base = msg.data
	}
}

// move moves the focus by one field, over the editable ones, wrapping.
func (f *form) move(by int) {
	first := 0
	if f.edit {
		first = 1
	}
	n := len(f.inputs) - first
	f.inputs[f.focus].Blur()
	f.focus = first + ((f.focus-first+by)%n+n)%n
	f.inputs[f.focus].Focus()
}

// formKey handles a key while a form is open: the focused field takes every
// key the form does not use, also q and the digits.
func (m *Model) formKey(k tea.KeyPressMsg) tea.Cmd {
	f, keys := m.form, m.keys
	switch {
	case key.Matches(k, keys.cancel):
		m.form = nil
	case key.Matches(k, keys.save), key.Matches(k, keys.enter) && f.focus == len(f.inputs)-1:
		return m.saveForm()
	case key.Matches(k, keys.nextField), key.Matches(k, keys.enter):
		f.move(1)
	case key.Matches(k, keys.prevField):
		f.move(-1)
	default:
		return m.formInput(k)
	}
	return nil
}

// formInput hands msg (a key or a paste) to the focused field.
func (m *Model) formInput(msg tea.Msg) tea.Cmd {
	f := m.form
	var cmd tea.Cmd
	f.inputs[f.focus], cmd = f.inputs[f.focus].Update(msg)
	return cmd
}

func (m *Model) saveForm() tea.Cmd {
	f := m.form
	if f.saving {
		return nil
	}
	v := make([]string, len(f.inputs))
	for i, in := range f.inputs {
		v[i] = strings.TrimSpace(in.Value())
	}
	apply, err := m.formEdit(f, v)
	if err != nil {
		f.err = p(err.Error())
		return nil
	}
	f.err, f.saving = "", true
	return m.saveCmd(f.id, f.ns, f.base, "saved "+formSpecs[f.kind].noun+" "+v[0], apply)
}

// formEdit turns the values of f into the edit of the file. It refuses an
// add of a name the broker has: the add would change that entry.
func (m *Model) formEdit(f *form, v []string) (func(*nsfile.File) error, error) {
	if !f.edit && m.has(f.kind, f.ns, v[0]) {
		return nil, fmt.Errorf("%s %q exists (e edits it)", formSpecs[f.kind].noun, v[0])
	}
	switch f.kind {
	case secretForm:
		var argv []string // none: config.Check says the command is empty
		if v[1] != "" {
			var err error
			if argv, err = SplitArgs(v[1]); err != nil {
				return nil, fmt.Errorf("command: %w", err)
			}
		}
		return func(nf *nsfile.File) error { return nf.SetSecret(v[0], argv) }, nil
	case routeForm:
		listen, err := port("listen", v[1])
		if err != nil {
			return nil, err
		}
		r := nsfile.Route{Name: v[0], Listen: listen, Upstream: v[2]}
		return func(nf *nsfile.File) error { return nf.SetRoute(r) }, nil
	default:
		listen, err := port("postgres listen", v[6])
		if err != nil {
			return nil, err
		}
		d := nsfile.Database{Alias: v[0], Upstream: v[1], DBName: v[2], User: v[3], Password: v[4], RequireAuth: v[5]}
		return func(nf *nsfile.File) error { return nf.SetDatabase(listen, d) }, nil
	}
}

func port(label, s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not a number", label, s)
	}
	return n, nil
}

// has tells whether the broker's ns has an entry of kind named name.
func (m *Model) has(kind formKind, ns, name string) bool {
	st := m.status(ns)
	if st == nil {
		return false
	}
	var names []string
	switch kind {
	case secretForm:
		for _, s := range st.Secrets {
			names = append(names, s.Name)
		}
	case routeForm:
		for _, r := range st.HTTP {
			names = append(names, r.Name)
		}
	default:
		if st.Postgres != nil {
			for _, d := range st.Postgres.Databases {
				names = append(names, d.Alias)
			}
		}
	}
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// askRemove asks for the y that removes name.
func (m *Model) askRemove(kind formKind, ns, name string) {
	m.hide()
	m.removal = &removal{kind: kind, ns: ns, name: name}
}

// removalKey removes on y; any other key cancels.
func (m *Model) removalKey(k tea.KeyPressMsg) tea.Cmd {
	r := m.removal
	m.removal = nil
	if !key.Matches(k, m.keys.confirm) {
		return nil
	}
	apply := func(nf *nsfile.File) error {
		switch r.kind {
		case secretForm:
			return nf.RemoveSecret(r.name)
		case routeForm:
			return nf.RemoveRoute(r.name)
		default:
			return nf.RemoveDatabase(r.name)
		}
	}
	m.edits++
	return m.saveCmd(m.edits, r.ns, nil, "removed "+formSpecs[r.kind].noun+" "+r.name, apply)
}

// saveCmd edits the namespace file of ns and, when that worked, sends
// reload. base is the file the edit started from; nil starts from the file
// as it is now.
func (m *Model) saveCmd(id int, ns string, base []byte, done string, apply func(*nsfile.File) error) tea.Cmd {
	dir, cl := m.opt.ConfigDir, m.cl
	return func() tea.Msg {
		msg := savedMsg{id: id, done: done, err: saveFile(dir, ns, base, apply)}
		if msg.err == nil {
			msg.reloadErr = cl.Reload()
		}
		return msg
	}
}

func saveFile(dir, ns string, base []byte, apply func(*nsfile.File) error) error {
	file := ns + ".yaml"
	path := filepath.Join(dir, file)
	nf, err := nsfile.Open(path)
	if err != nil {
		return err
	}
	if base != nil {
		// Read after Open: Save refuses a file that is no longer the one
		// Open read, so a match here means the save starts from base.
		now, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(now, base) {
			return nsfile.ErrChanged
		}
	}
	if err := apply(nf); err != nil {
		return err
	}
	return nf.Save(func(data []byte) error { return config.Check(dir, file, data) })
}

// gotSaved closes the form of a save that worked or found the file changed,
// and keeps it open with the error otherwise.
func (m *Model) gotSaved(msg savedMsg) tea.Cmd {
	f := m.form
	mine := f != nil && f.id == msg.id
	switch {
	case msg.err == nil:
		if mine {
			m.form = nil
		}
		m.note = p(msg.done)
		if msg.reloadErr != nil {
			m.note += "; reload: " + p(msg.reloadErr.Error())
		}
		return m.statusOnce()
	case mine && !errors.Is(msg.err, nsfile.ErrChanged):
		f.saving, f.err = false, p(msg.err.Error())
		return nil
	default:
		// The file changed (the TUI shows what the broker loaded: the next
		// status brings it), or a remove failed.
		if mine {
			m.form = nil
		}
		m.note = p(msg.err.Error())
		return m.statusOnce()
	}
}

// statusOnce asks for a status now, outside the 2 s poll chain.
func (m *Model) statusOnce() tea.Cmd {
	cl := m.cl
	return func() tea.Msg {
		nss, err := cl.Status("")
		return statusMsg{nss: nss, err: err, once: true}
	}
}

// editorCmd gives the terminal to the owner's editor on the file of ns. The
// editor runs in the config dir, never in a project folder, as
// `sbx-broker edit` does.
func (m *Model) editorCmd(ns string) tea.Cmd {
	if m.opt.Editor == nil {
		m.note = "no editor"
		return nil
	}
	getenv := m.opt.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	e := &editorExec{run: m.opt.Editor, dir: m.opt.ConfigDir, argv: EditorArgv(getenv, m.nsPath(ns))}
	return m.exec(e, func(err error) tea.Msg { return editedMsg{err: err} })
}

// gotEdited reloads after the editor, which may have changed the file.
func (m *Model) gotEdited(msg editedMsg) tea.Cmd {
	if msg.err != nil {
		m.note = "editor: " + p(msg.err.Error())
	}
	cl := m.cl
	return func() tea.Msg {
		if err := cl.Reload(); err != nil {
			return reloadedMsg{err: err}
		}
		nss, err := cl.Status("")
		return statusMsg{nss: nss, err: err, once: true}
	}
}

func (m *Model) gotReloaded(msg reloadedMsg) {
	if errors.Is(msg.err, admin.ErrNoBroker) {
		m.gone = true
		return
	}
	m.note = "reload: " + p(msg.err.Error())
}

// secretsEditKey handles the edit keys of the Secrets view.
func (m *Model) secretsEditKey(k tea.KeyPressMsg, ns *admin.NamespaceStatus) tea.Cmd {
	at := m.secCursor < len(ns.Secrets)
	switch {
	case key.Matches(k, m.keys.add):
		return m.openForm(secretForm, ns.Name, false)
	case key.Matches(k, m.keys.edit) && at:
		s := ns.Secrets[m.secCursor]
		return m.openForm(secretForm, ns.Name, true, s.Name, JoinArgs(s.Command))
	case key.Matches(k, m.keys.remove) && at:
		m.askRemove(secretForm, ns.Name, ns.Secrets[m.secCursor].Name)
	case key.Matches(k, m.keys.editor):
		return m.editorCmd(ns.Name)
	}
	return nil
}

// routesKey handles the keys of the Routes view. Its rows are the HTTP
// routes, then the databases.
func (m *Model) routesKey(k tea.KeyPressMsg) tea.Cmd {
	ns := m.current()
	if ns == nil {
		return nil
	}
	var pg admin.PostgresInfo
	if ns.Postgres != nil {
		pg = *ns.Postgres
	}
	n := len(ns.HTTP) + len(pg.Databases)
	at := m.routeCursor < n
	switch {
	case key.Matches(k, m.keys.up):
		m.routeCursor = max(m.routeCursor-1, 0)
	case key.Matches(k, m.keys.down):
		m.routeCursor = min(m.routeCursor+1, max(n-1, 0))
	case key.Matches(k, m.keys.addRoute):
		return m.openForm(routeForm, ns.Name, false)
	case key.Matches(k, m.keys.addDatabase):
		listen := ""
		if ns.Postgres != nil {
			listen = strconv.Itoa(pg.Listen)
		}
		return m.openForm(databaseForm, ns.Name, false, "", "", "", "", "", "", listen)
	case key.Matches(k, m.keys.edit) && at:
		if i := m.routeCursor; i < len(ns.HTTP) {
			r := ns.HTTP[i]
			return m.openForm(routeForm, ns.Name, true, r.Name, strconv.Itoa(r.Listen), r.Upstream)
		}
		d := pg.Databases[m.routeCursor-len(ns.HTTP)]
		return m.openForm(databaseForm, ns.Name, true, d.Alias, d.Upstream, d.DBName, d.User, d.Password,
			d.RequireAuth, strconv.Itoa(pg.Listen))
	case key.Matches(k, m.keys.remove) && at:
		if i := m.routeCursor; i < len(ns.HTTP) {
			m.askRemove(routeForm, ns.Name, ns.HTTP[i].Name)
		} else {
			m.askRemove(databaseForm, ns.Name, pg.Databases[i-len(ns.HTTP)].Alias)
		}
	case key.Matches(k, m.keys.editor):
		return m.editorCmd(ns.Name)
	}
	return nil
}

// formView renders the open form in place of the view.
func (m *Model) formView() string {
	f := m.form
	spec := formSpecs[f.kind]
	title := "Add " + spec.a + " to " + p(f.ns)
	if f.edit {
		title = "Edit " + spec.noun + " " + p(f.inputs[0].Value()) + " of " + p(f.ns)
	}
	rows := make([][]string, len(f.inputs))
	for i, in := range f.inputs {
		field := in.View()
		if f.edit && i == 0 {
			field = p(in.Value()) + "  (fixed)"
		}
		rows[i] = []string{spec.labels[i], field}
	}
	out := title + "\n\n" + table(nil, rows, f.focus)
	switch {
	case f.saving:
		out += "\n\nsaving…"
	case f.err != "":
		out += "\n\n" + f.err
	}
	return out
}

// removalLine asks for the y of a remove.
func (m *Model) removalLine() string {
	r := m.removal
	return "Remove " + formSpecs[r.kind].noun + " " + p(r.name) + " from " + p(r.ns) + "? (y/n)"
}
