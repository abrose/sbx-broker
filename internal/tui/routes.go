package tui

import (
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/abrose/sbx-broker/internal/admin"
)

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
		return m.openForm(routeForm, ns.Name, "")
	case key.Matches(k, m.keys.addDatabase):
		return m.openForm(databaseForm, ns.Name, "")
	case key.Matches(k, m.keys.edit) && at:
		if i := m.routeCursor; i < len(ns.HTTP) {
			return m.openForm(routeForm, ns.Name, ns.HTTP[i].Name)
		}
		return m.openForm(databaseForm, ns.Name, pg.Databases[m.routeCursor-len(ns.HTTP)].Alias)
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

// routesView shows the HTTP routes and the databases of the namespace of
// Secrets, as the broker loaded them. Its rows are the routes, then the
// databases.
func (m *Model) routesView() string {
	ns := m.current()
	switch {
	case ns == nil && m.ns == "":
		return "no namespace (pick one in 1 Namespaces)"
	case ns == nil:
		return "Routes of " + p(m.ns) + "\n\nthe broker has no namespace " + p(m.ns)
	}
	or := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}
	out := "Routes of " + p(ns.Name) + "\n\nHTTP\n"
	if len(ns.HTTP) == 0 {
		out += "no HTTP routes"
	} else {
		rows := make([][]string, len(ns.HTTP))
		for i, r := range ns.HTTP {
			rows[i] = []string{p(r.Name), strconv.Itoa(r.Listen), p(r.Upstream), or(p(strings.Join(r.Transforms, ", ")))}
		}
		out += table([]string{"NAME", "LISTEN", "UPSTREAM", "TRANSFORMS"}, rows, rowIn(m.routeCursor, 0, len(rows)))
	}
	if ns.Postgres == nil || len(ns.Postgres.Databases) == 0 {
		return out + "\n\nPOSTGRES\nno Postgres databases"
	}
	pg := ns.Postgres
	rows := make([][]string, len(pg.Databases))
	for i, d := range pg.Databases {
		rows[i] = []string{p(d.Alias), p(d.Upstream), p(d.DBName), p(d.User), p(d.Password), or(p(d.RequireAuth))}
	}
	return out + "\n\nPOSTGRES " + strconv.Itoa(pg.Listen) + "\n" +
		table([]string{"ALIAS", "UPSTREAM", "DBNAME", "USER", "PASSWORD", "REQUIRE-AUTH"}, rows, rowIn(m.routeCursor, len(ns.HTTP), len(rows)))
}

// routeRows is the number of rows of the Routes view of ns.
func routeRows(ns *admin.NamespaceStatus) int {
	n := len(ns.HTTP)
	if ns.Postgres != nil {
		n += len(ns.Postgres.Databases)
	}
	return n
}

// rowIn is the cursor of a table whose n rows start at row from of the view:
// n (no row marked, the rows still indented) when the cursor is elsewhere.
func rowIn(cursor, from, n int) int {
	if cursor >= from && cursor < from+n {
		return cursor - from
	}
	return n
}
