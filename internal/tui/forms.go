package tui

import (
	"errors"
	"fmt"
	"path/filepath"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/abrose/sbx-broker/internal/admin"
	"github.com/abrose/sbx-broker/internal/nsfile"
)

// The edit forms write the namespace file themselves (designer's call C2),
// only through nsfile: it keeps the comments and the key order, refuses a file
// that changed on disk, runs config.Check and writes atomically. Then the TUI
// sends reload. A name or alias is fixed after creation, and transforms are
// not in a form: o opens the file in the owner's editor for them (C3).

type formKind int

const (
	secretForm formKind = iota
	routeForm
	databaseForm
)

// The index of each field of a form, in the order of its labels in formSpecs.
const (
	secretName = iota
	secretCommand
	secretFields // the number of fields
)

const (
	routeName = iota
	routeListen
	routeUpstream
	routeFields
)

const (
	dbAlias = iota
	dbUpstream
	dbName
	dbUser
	dbPassword
	dbRequireAuth
	dbListen
	dbFields
)

// formSpecs name each kind and list the fields of its form, in order. The
// first field is the name or alias.
var formSpecs = [...]struct {
	noun, a, word string // "HTTP route", "an HTTP route", "route"
	labels        []string
	file          nsfile.Kind
}{
	secretForm:   {"secret", "a secret", "secret", []string{"name", "command"}, nsfile.KindSecret},
	routeForm:    {"HTTP route", "an HTTP route", "route", []string{"name", "listen port", "upstream"}, nsfile.KindRoute},
	databaseForm: {"database", "a database", "database", []string{"alias", "upstream", "dbname", "user", "password secret", "require-auth", "postgres listen port"}, nsfile.KindDatabase},
}

// form is an open add or edit form.
type form struct {
	id   int
	kind formKind
	edit bool // the name or alias, the first field, is fixed (C3)
	ns   string
	// base is the namespace file as the form read it when it opened; nil
	// until the read arrives, and after a failed read. A save needs it, and
	// refuses when the file changed since.
	base   []byte
	inputs []textinput.Model
	focus  int
	err    string // why the last save failed, printable
	saving bool
}

// formBaseMsg is the message of the form's read of the file.
type formBaseMsg struct {
	id     int
	data   []byte   // the file as nsfile read it
	values []string // the fields from the file; nil: none
	err    error    // the read failed, or the entry of an edit is gone
}

func (m *Model) nsPath(ns string) string { return filepath.Join(m.opt.ConfigDir, ns+".yaml") }

// openForm opens a form for ns: an edit form of the entry name, or with name
// "" an add form. It reads the namespace file once, the bytes the save will
// start from, and fills the fields from that read: an edit form all of them,
// a database add form postgres listen. The broker's status does not fill
// them: it lags a save in the editor by about 2 s, and a save from it would
// put the old values back.
func (m *Model) openForm(kind formKind, ns, name string) tea.Cmd {
	m.hide()
	m.edits++
	edit := name != ""
	f := &form{id: m.edits, kind: kind, edit: edit, ns: ns}
	for range formSpecs[kind].labels {
		in := textinput.New()
		in.Prompt = ""
		st := in.Styles()
		st.Cursor.Blink = false // a blinking cursor would send a tick every half second
		in.SetStyles(st)
		// ctrl+v would read the system clipboard (often a password) into a
		// message the TUI drops: a value it holds but never shows. A paste
		// of the terminal still arrives as a tea.PasteMsg.
		in.KeyMap.Paste.SetEnabled(false)
		f.inputs = append(f.inputs, in)
	}
	if edit {
		f.inputs[0].SetValue(name)
		f.focus = 1
	}
	f.inputs[f.focus].Focus()
	m.form = f
	path, id := m.nsPath(ns), f.id
	return func() tea.Msg {
		nf, err := nsfile.Open(path)
		if err != nil {
			return formBaseMsg{id: id, err: err}
		}
		values, err := formValues(kind, name, nf)
		if err != nil {
			return formBaseMsg{id: id, err: err}
		}
		return formBaseMsg{id: id, data: nf.Source(), values: values}
	}
}

// formValues returns the fields of a form of kind for the entry name as nf
// writes them. An add form (name "") gets only a database form's postgres
// listen; its other fields are "".
func formValues(kind formKind, name string, nf *nsfile.File) ([]string, error) {
	if name == "" {
		if kind == databaseForm {
			v := make([]string, dbFields)
			v[dbListen] = nf.PostgresListen()
			return v, nil
		}
		return nil, nil
	}
	var v []string
	var err error
	switch kind {
	case secretForm:
		var argv []string
		argv, err = nf.Secret(name)
		v = make([]string, secretFields)
		v[secretName], v[secretCommand] = name, JoinArgs(argv)
	case routeForm:
		var listen, upstream string
		listen, upstream, err = nf.Route(name)
		v = make([]string, routeFields)
		v[routeName], v[routeListen], v[routeUpstream] = name, listen, upstream
	default:
		var d nsfile.Database
		var listen string
		d, listen, err = nf.Database(name)
		v = make([]string, dbFields)
		v[dbAlias], v[dbUpstream], v[dbName], v[dbUser] = name, d.Upstream, d.DBName, d.User
		v[dbPassword], v[dbRequireAuth], v[dbListen] = d.Password, d.RequireAuth, listen
	}
	if errors.Is(err, nsfile.ErrNotFound) {
		return nil, fmt.Errorf("%s %s is no longer in the file", formSpecs[kind].word, name)
	}
	return v, err
}

// gotBase takes the read of the file: its bytes become the base of the save,
// and its values fill the fields (of an add form only those it has). A failed
// read leaves no base: the form shows why and saves nothing.
func (m *Model) gotBase(msg formBaseMsg) {
	f := m.form
	if f == nil || f.id != msg.id {
		return
	}
	if msg.err != nil {
		f.err = p(msg.err.Error())
		return
	}
	f.base = msg.data
	for i, v := range msg.values {
		if i < len(f.inputs) && (f.edit || v != "") {
			f.inputs[i].SetValue(v)
		}
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

// secretsEditKey handles the edit keys of the Secrets view.
func (m *Model) secretsEditKey(k tea.KeyPressMsg, ns *admin.NamespaceStatus) tea.Cmd {
	at := m.secCursor < len(ns.Secrets)
	switch {
	case key.Matches(k, m.keys.add):
		return m.openForm(secretForm, ns.Name, "")
	case key.Matches(k, m.keys.edit) && at:
		return m.openForm(secretForm, ns.Name, ns.Secrets[m.secCursor].Name)
	case key.Matches(k, m.keys.remove) && at:
		m.askRemove(secretForm, ns.Name, ns.Secrets[m.secCursor].Name)
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
	case f.base == nil:
		out += "\n\nreading the file…"
	}
	return out
}

// removalLine asks for the y of a remove.
func (m *Model) removalLine() string {
	r := m.removal
	return "Remove " + formSpecs[r.kind].noun + " " + p(r.name) + " from " + p(r.ns) + "? (y/n)"
}
