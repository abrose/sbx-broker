package tui

import (
	"errors"
	"io"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/abrose/sbx-broker/internal/admin"
)

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

type (
	editedMsg   struct{ err error } // the editor ended
	reloadedMsg struct{ err error } // the reload after the editor failed
)

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
		m.down = NoBroker
		return
	}
	m.note = "reload: " + p(msg.err.Error())
}
