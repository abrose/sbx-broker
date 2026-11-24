package tui

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/abrose/sbx-broker/internal/config"
	"github.com/abrose/sbx-broker/internal/nsfile"
)

// removal is a remove that waits for its y.
type removal struct {
	kind     formKind
	ns, name string
}

type savedMsg struct {
	id        int    // the form or removal that saved
	done      string // what the save did, for the note: "saved secret x"
	err       error  // the save failed: nothing was written
	reloadErr error  // the save worked; the reload did not
}

func (m *Model) saveForm() tea.Cmd {
	f := m.form
	if f.saving || f.base == nil {
		return nil // a save needs the read of the file: the form shows that it reads, or why the read failed
	}
	v := make([]string, len(f.inputs))
	for i, in := range f.inputs {
		v[i] = strings.TrimSpace(in.Value())
	}
	apply, err := formEdit(f, v)
	if err != nil {
		f.err = p(err.Error())
		return nil
	}
	f.err, f.saving = "", true
	return m.saveCmd(f.id, f.ns, f.base, "saved "+formSpecs[f.kind].noun+" "+v[0], apply)
}

// formEdit turns the values of f into the edit of the file. The nsfile Set
// methods add or replace, so the edit asks the opened file first: an add
// refuses a name the file holds, and an edit refuses an entry that left it.
// The broker's status can lag the file, so it does not decide.
func formEdit(f *form, v []string) (func(*nsfile.File) error, error) {
	set, err := formSet(f.kind, v)
	if err != nil {
		return nil, err
	}
	spec, edit, name := formSpecs[f.kind], f.edit, v[0]
	return func(nf *nsfile.File) error {
		switch have := nf.Has(spec.file, name); {
		case have && !edit:
			return fmt.Errorf("%s %q exists (e edits it)", spec.noun, name)
		case !have && edit:
			return fmt.Errorf("%s %s is no longer in the file", spec.word, name)
		}
		return set(nf)
	}, nil
}

// formSet parses the values of a form of kind into the Set call it makes.
func formSet(kind formKind, v []string) (func(*nsfile.File) error, error) {
	switch kind {
	case secretForm:
		var argv []string // none: config.Check says the command is empty
		if v[secretCommand] != "" {
			var err error
			if argv, err = SplitArgs(v[secretCommand]); err != nil {
				return nil, fmt.Errorf("command: %w", err)
			}
		}
		return func(nf *nsfile.File) error { return nf.SetSecret(v[secretName], argv) }, nil
	case routeForm:
		listen, err := port("listen port", v[routeListen])
		if err != nil {
			return nil, err
		}
		r := nsfile.Route{Name: v[routeName], Listen: listen, Upstream: v[routeUpstream]}
		return func(nf *nsfile.File) error { return nf.SetRoute(r) }, nil
	default:
		listen, err := port("postgres listen port", v[dbListen])
		if err != nil {
			return nil, err
		}
		d := nsfile.Database{Alias: v[dbAlias], Upstream: v[dbUpstream], DBName: v[dbName], User: v[dbUser], Password: v[dbPassword], RequireAuth: v[dbRequireAuth]}
		return func(nf *nsfile.File) error { return nf.SetDatabase(listen, d) }, nil
	}
}

func port(label, s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not a port number", label, s)
	}
	return n, nil
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
