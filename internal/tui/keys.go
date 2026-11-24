package tui

import (
	"charm.land/bubbles/v2/key"
)

type keyMap struct {
	views               []key.Binding // 1 to 4, in the order of tabs
	next, prev, quit    key.Binding
	forceQuit           key.Binding
	up, down            key.Binding
	open, inspect       key.Binding
	refresh, refreshAll key.Binding
	reveal, confirm     key.Binding
	back, viewsHelp     key.Binding
	nsFilter            key.Binding // the Log view
	kindFilter, clear   key.Binding
	newest              key.Binding
	add, addRoute       key.Binding // the edit keys of Secrets and Routes
	addDatabase, edit   key.Binding
	remove, editor      key.Binding
	nextField           key.Binding // in a form
	prevField, enter    key.Binding
	save, cancel        key.Binding
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
		prev: b([]string{"shift+tab"}, "shift+tab", "previous view"),
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
		nsFilter:   b([]string{"n"}, "n", "namespace filter"),
		kindFilter: b([]string{"t"}, "t", "kind filter"),
		clear:      b([]string{"c"}, "c", "clear filters"),
		newest:     b([]string{"G"}, "G", "newest"),
		viewsHelp:  b([]string{"1", "2", "3", "4", "tab", "shift+tab"}, "1-4/tab/shift+tab", "views"), // for the footer only

		add:         b([]string{"a"}, "a", "add"),
		addRoute:    b([]string{"a"}, "a", "add route"),
		addDatabase: b([]string{"p"}, "p", "add database"),
		edit:        b([]string{"e"}, "e", "edit"),
		remove:      b([]string{"d"}, "d", "remove"),
		editor:      b([]string{"o"}, "o", "open in editor"),
		nextField:   b([]string{"tab"}, "tab", "next field"),
		prevField:   b([]string{"shift+tab"}, "shift+tab", "previous field"),
		enter:       b([]string{"enter"}, "enter", "next field"),
		save:        b([]string{"ctrl+s"}, "enter/ctrl+s", "save"), // enter saves on the last field
		cancel:      b([]string{"esc"}, "esc", "cancel"),
	}
}

// footerKeys are the keys of the current view.
func (m *Model) footerKeys() []key.Binding {
	k := m.keys
	switch {
	case m.form != nil:
		return []key.Binding{k.nextField, k.prevField, k.save, k.cancel, k.forceQuit}
	case m.removal != nil:
		return []key.Binding{k.confirm, k.forceQuit}
	}
	var own []key.Binding
	switch m.view {
	case viewNamespaces:
		own = []key.Binding{k.up, k.down, k.open}
	case viewSecrets:
		own = []key.Binding{k.up, k.down, k.inspect, k.refresh, k.refreshAll, k.add, k.edit, k.remove, k.editor}
	case viewRoutes:
		own = []key.Binding{k.up, k.down, k.addRoute, k.addDatabase, k.edit, k.remove, k.editor}
	case viewInspect:
		own = []key.Binding{k.reveal, k.back}
	case viewLog:
		own = []key.Binding{k.up, k.down, k.newest, k.nsFilter, k.kindFilter, k.clear}
	}
	return append(own, k.viewsHelp, k.quit)
}
