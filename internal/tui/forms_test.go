package tui

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/abrose/sbx-broker/internal/admin"
	"github.com/abrose/sbx-broker/internal/config"
	"github.com/abrose/sbx-broker/internal/nsfile"
)

// acmeFile matches fixture(): the secrets, the route and the database of acme.
const acmeFile = `# acme: the test namespace
secrets:
  api-key: # the main key
    command: ["printf", "k"]
  db-pass:
    command: ["printf", "p"]

# the api route
http:
  - name: api
    listen: 29610
    upstream: https://api.example.com
    transforms: [swap]

postgres:
  listen: 29620
  databases:
    main: # the app database
      upstream: 127.0.0.1:5432
      dbname: appdb
      user: appuser
      password: db-pass
`

var acmeComments = []string{"# acme: the test namespace", "# the main key", "# the api route", "# the app database"}

// configDir returns a config dir (0700) with acme.yaml holding content.
func configDir(t *testing.T, content string) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir, writeNamespace(t, dir, "acme", content)
}

func writeNamespace(t *testing.T, dir, ns, content string) string {
	t.Helper()
	path := filepath.Join(dir, ns+".yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// startForms starts the TUI on the Secrets of acme, with acme.yaml in a real
// config dir and the fake client.
func startForms(t *testing.T) (h *harness, dir, path string) {
	t.Helper()
	dir, path = configDir(t, acmeFile)
	return start(t, Options{ConfigDir: dir, Namespace: "acme"}), dir, path
}

// load returns acme as the broker would load it from dir.
func load(t *testing.T, dir string) *config.Namespace {
	t.Helper()
	ok, bad, err := config.LoadDir(dir)
	if err != nil || len(bad) != 0 || ok["acme"] == nil {
		t.Fatalf("load: %v %v", bad, err)
	}
	return ok["acme"]
}

func secretOf(ns *config.Namespace, name string) *config.Secret {
	for i := range ns.Secrets {
		if ns.Secrets[i].Name == name {
			return &ns.Secrets[i]
		}
	}
	return nil
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (h *harness) keepsComments(path string) {
	h.t.Helper()
	data := read(h.t, path)
	for _, c := range acmeComments {
		if !strings.Contains(data, c) {
			h.t.Fatalf("the file lost %q:\n%s", c, data)
		}
	}
}

func (h *harness) reloads() int {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	return h.fake.reloads
}

func TestAddSecretWritesFileAndReloads(t *testing.T) {
	h, dir, path := startForms(t)
	h.keys("a")
	h.has("Add a secret to acme", "name", "command")
	h.typeText("new-key")
	h.keys("tab")
	h.typeText(`printf '%s' v1`)
	h.keys("enter")
	if h.m.form != nil {
		t.Fatalf("the form is still open:\n%s", h.screen())
	}
	s := secretOf(load(t, dir), "new-key")
	if s == nil || !reflect.DeepEqual(s.Command, []string{"printf", "%s", "v1"}) {
		t.Fatalf("secret %+v", s)
	}
	if n := h.reloads(); n != 1 {
		t.Fatalf("%d reloads, want 1", n)
	}
	h.keepsComments(path)
	h.has("saved secret new-key")
}

func TestEditSecretKeepsComments(t *testing.T) {
	h, dir, path := startForms(t)
	h.keys("e")
	h.has("Edit secret api-key of acme", "op read 'op://<vault>/<item>/credential'")
	// The name is fixed (C3): the command has the focus, and q and digits
	// are text there, not a quit or a view switch.
	h.keys("ctrl+u")
	h.typeText("printf q1")
	h.keys("shift+tab", "shift+tab") // the name is skipped: the command keeps the focus
	h.typeText(" 2")
	h.keys("ctrl+s")
	if h.quit || h.m.form != nil {
		t.Fatalf("quit %v, form %v:\n%s", h.quit, h.m.form != nil, h.screen())
	}
	s := secretOf(load(t, dir), "api-key")
	if s == nil || !reflect.DeepEqual(s.Command, []string{"printf", "q1", "2"}) {
		t.Fatalf("secret %+v", s)
	}
	h.keepsComments(path)
}

func TestFormErrorKeepsInput(t *testing.T) {
	h, _, path := startForms(t)
	h.keys("a")
	h.typeText("x")
	h.keys("tab", "enter")
	h.has(`secret "x": command is empty`)
	if h.m.form == nil || h.m.form.inputs[0].Value() != "x" {
		t.Fatalf("the form lost its input:\n%s", h.screen())
	}
	if read(t, path) != acmeFile {
		t.Fatal("the file changed")
	}
	if n := h.reloads(); n != 0 {
		t.Fatalf("%d reloads", n)
	}
	// A broken command line is an error of the form; the file is not read.
	h.typeText(`printf 'x`)
	h.keys("enter")
	h.has("command: unclosed quote")
}

func TestPortClashShownInForm(t *testing.T) {
	h, dir, path := startForms(t)
	writeNamespace(t, dir, "other", "http:\n  - name: web\n    listen: 29700\n    upstream: https://api.example.com\n")
	h.keys("3", "a")
	h.has("Add an HTTP route to acme", "listen", "upstream")
	h.typeText("web")
	h.keys("tab")
	h.typeText("29700")
	h.keys("tab")
	h.typeText("https://api.example.com")
	h.keys("enter")
	h.has("port 29700 is used by namespace other")
	if h.m.form == nil {
		t.Fatal("the form closed")
	}
	if read(t, path) != acmeFile {
		t.Fatal("the file changed")
	}
}

func TestRemoveNeedsConfirmation(t *testing.T) {
	h, dir, path := startForms(t)
	h.keys("d")
	h.has("Remove secret api-key from acme? (y/n)")
	h.keys("n")
	h.hasNot("Remove secret")
	if read(t, path) != acmeFile || h.reloads() != 0 {
		t.Fatal("n removed the secret")
	}
	h.keys("d", "y")
	if secretOf(load(t, dir), "api-key") != nil {
		t.Fatal("the secret is still there")
	}
	if n := h.reloads(); n != 1 {
		t.Fatalf("%d reloads, want 1", n)
	}
	h.has("removed secret api-key")
	data := read(t, path)
	for _, c := range acmeComments {
		if c != "# the main key" && !strings.Contains(data, c) { // the comment of api-key goes with it
			t.Fatalf("the file lost %q:\n%s", c, data)
		}
	}
}

func TestRemoveSecretInUseShowsConfigError(t *testing.T) {
	h, _, path := startForms(t)
	h.keys("j", "d", "y")
	h.has(`password names the undefined secret "db-pass"`)
	if read(t, path) != acmeFile {
		t.Fatal("the file changed")
	}
}

func TestChangedFileClosesForm(t *testing.T) {
	h, _, path := startForms(t)
	h.keys("a")
	changed := acmeFile + "# added in the editor\n"
	writeNamespace(t, filepath.Dir(path), "acme", changed)
	h.typeText("new-key")
	h.keys("tab")
	h.typeText("printf v")
	h.keys("enter")
	h.has(nsfile.ErrChanged.Error())
	if h.m.form != nil {
		t.Fatal("the form is still open")
	}
	if read(t, path) != changed {
		t.Fatal("the save overwrote the change")
	}
}

func TestAddDatabaseCreatesPostgresBlock(t *testing.T) {
	file := strings.Split(acmeFile, "\npostgres:")[0] + "\n"
	dir, path := configDir(t, file)
	nss := fixture()
	nss[0].Postgres = nil
	h := newHarness(t, &fakeClient{nss: nss}, Options{ConfigDir: dir, Namespace: "acme"})
	h.keys("3", "p")
	h.has("Add a database to acme", "alias", "upstream", "dbname", "user", "password secret", "require-auth", "postgres listen")
	for i, v := range []string{"main", "127.0.0.1:5432", "appdb", "appuser", "db-pass", "scram-sha-256"} {
		if i > 0 {
			h.keys("tab")
		}
		h.typeText(v)
	}
	h.keys("tab")
	h.typeText("29620")
	h.keys("enter")
	pg := load(t, dir).Postgres
	want := config.Database{Alias: "main", Upstream: "127.0.0.1:5432", DBName: "appdb", User: "appuser",
		Password: "db-pass", RequireAuth: config.RequireAuthSCRAM}
	if pg == nil || pg.Listen != 29620 || len(pg.Databases) != 1 || pg.Databases[0] != want {
		t.Fatalf("postgres %+v:\n%s", pg, read(t, path))
	}
	if !strings.Contains(read(t, path), "# the api route") {
		t.Fatal("the file lost its comments")
	}
}

// The database form fills postgres listen with the current port.
func TestDatabaseFormFillsListen(t *testing.T) {
	h, _, _ := startForms(t)
	h.keys("3", "p")
	if got := h.m.form.inputs[6].Value(); got != "29620" {
		t.Fatalf("postgres listen %q", got)
	}
}

func TestEditRouteKeepsTransforms(t *testing.T) {
	h, dir, path := startForms(t)
	h.keys("3", "e")
	h.has("Edit HTTP route api of acme")
	h.keys("tab", "ctrl+u")
	h.typeText("https://api2.example.com")
	h.keys("enter")
	r := load(t, dir).HTTP[0]
	if r.Upstream.String() != "https://api2.example.com" || r.Listen != 29610 || len(r.Transforms) != 1 || r.Transforms[0].Name != "swap" {
		t.Fatalf("route %+v", r)
	}
	if !strings.Contains(read(t, path), "transforms: [swap]") {
		t.Fatalf("the transforms changed:\n%s", read(t, path))
	}
	h.keepsComments(path)
}

func TestEditAndRemoveDatabase(t *testing.T) {
	h, dir, path := startForms(t)
	h.keys("3", "j", "e")
	h.has("Edit database main of acme")
	for range 2 {
		h.keys("tab")
	}
	h.keys("ctrl+u")
	h.typeText("other")
	h.keys("ctrl+s")
	if db := load(t, dir).Postgres.Databases[0]; db.User != "other" || db.DBName != "appdb" {
		t.Fatalf("database %+v", db)
	}
	h.keys("d")
	h.has("Remove database main from acme? (y/n)")
	h.keys("y")
	if pg := load(t, dir).Postgres; pg != nil {
		t.Fatalf("postgres %+v", pg)
	}
	if strings.Contains(read(t, path), "postgres") {
		t.Fatalf("the postgres block is still there:\n%s", read(t, path))
	}
}

func TestAddRefusesExistingName(t *testing.T) {
	h, _, path := startForms(t)
	h.keys("a")
	h.typeText("api-key")
	h.keys("tab")
	h.typeText("printf x")
	h.keys("enter")
	h.has(`secret "api-key" exists (e edits it)`)
	if read(t, path) != acmeFile {
		t.Fatal("the file changed")
	}
}

// The file decides, not the broker's status: a name added in the editor that
// the broker has not loaded yet is refused all the same.
func TestAddRefusesNameOnlyInFile(t *testing.T) {
	h, dir, path := startForms(t)
	inFile := strings.Replace(acmeFile, "  db-pass:\n", "  extra:\n    command: [\"printf\", \"e\"]\n  db-pass:\n", 1)
	writeNamespace(t, dir, "acme", inFile)
	h.keys("a")
	h.typeText("extra")
	h.keys("tab")
	h.typeText("printf x")
	h.keys("enter")
	h.has(`secret "extra" exists (e edits it)`)
	if h.m.form == nil {
		t.Fatal("the form closed")
	}
	if read(t, path) != inFile || h.reloads() != 0 {
		t.Fatal("the add changed the file")
	}
}

// An edit of an entry that left the file (the broker has not seen it go
// yet) does not create it again.
func TestEditRefusesEntryGoneFromFile(t *testing.T) {
	h, dir, path := startForms(t)
	gone := strings.Replace(acmeFile, "  api-key: # the main key\n    command: [\"printf\", \"k\"]\n", "", 1)
	writeNamespace(t, dir, "acme", gone)
	h.keys("e")
	h.has("Edit secret api-key of acme")
	h.keys("ctrl+s")
	h.has("secret api-key is no longer in the file")
	if h.m.form == nil {
		t.Fatal("the form closed")
	}
	if read(t, path) != gone || h.reloads() != 0 {
		t.Fatal("the edit changed the file")
	}

	h.keys("esc")
	writeNamespace(t, dir, "acme", strings.Split(gone, "\n# the api route")[0]+"\n")
	h.keys("3", "e", "ctrl+s")
	h.has("route api is no longer in the file")
}

// ctrl+v must not read the system clipboard: it often holds a password, and
// the TUI never holds a value it does not show. A terminal paste still works.
func TestCtrlVDoesNotReadClipboard(t *testing.T) {
	h, _, _ := startForms(t)
	h.keys("a")
	if _, cmd := h.m.Update(keyPress("ctrl+v")); cmd != nil {
		t.Fatal("ctrl+v returned a command (a clipboard read)")
	}
	h.send(tea.PasteMsg{Content: "pasted-name"})
	if got := h.m.form.inputs[0].Value(); got != "pasted-name" {
		t.Fatalf("field %q after a terminal paste", got)
	}
}

func TestFormEscCancels(t *testing.T) {
	h, _, path := startForms(t)
	h.keys("a")
	h.typeText("x")
	h.keys("esc")
	if h.m.form != nil || read(t, path) != acmeFile {
		t.Fatal("esc did not cancel")
	}
	h.has("Secrets of acme")
}

func TestRoutesView(t *testing.T) {
	h, _, _ := startForms(t)
	h.keys("3")
	h.has("Routes of acme", "HTTP", "NAME", "LISTEN", "UPSTREAM", "TRANSFORMS",
		"api", "29610", "https://api.example.com", "swap",
		"POSTGRES 29620", "ALIAS", "DBNAME", "USER", "PASSWORD", "REQUIRE-AUTH",
		"main", "127.0.0.1:5432", "appdb", "appuser", "db-pass")
	h.hasNot("not in this version yet")

	nss := fixture()
	nss[0].Postgres.Databases[0].User = "app\x1b[2J"
	h.fake.nss = nss
	h.send(h.lastTimer(2 * time.Second).msg)
	h.has(`app\x1b[2J`)
	h.hasNot("\x1b[2J")

	nss = fixture()
	nss[0].HTTP, nss[0].Postgres = nil, nil
	h.fake.nss = nss
	h.send(h.lastTimer(2 * time.Second).msg)
	h.has("no HTTP routes", "no Postgres databases")
}

// editorCall is what a fake Options.Editor got.
type editorCall struct {
	dir  string
	argv []string
	term Terminal
}

func TestOpenEditorKey(t *testing.T) {
	dir, path := configDir(t, acmeFile)
	var calls []editorCall
	env := map[string]string{"VISUAL": "code --wait"}
	opt := Options{ConfigDir: dir, Namespace: "acme",
		Getenv: func(k string) string { return env[k] },
		Editor: func(dir string, argv []string, term Terminal) error {
			calls = append(calls, editorCall{dir, argv, term})
			return nil
		}}
	h := start(t, opt)
	h.keys("o")
	want := []string{"code", "--wait", path}
	if len(calls) != 1 || calls[0].dir != dir || !reflect.DeepEqual(calls[0].argv, want) {
		t.Fatalf("calls %+v", calls)
	}
	if n := h.reloads(); n != 1 {
		t.Fatalf("%d reloads after the editor, want 1", n)
	}
	h.keys("3", "o")
	if len(calls) != 2 {
		t.Fatalf("o in Routes: calls %+v", calls)
	}
}

// Bubble Tea hands its terminal to the adapter, which hands it to the Editor.
func TestEditorGetsTerminal(t *testing.T) {
	var got Terminal
	e := &editorExec{dir: "d", argv: []string{"vi", "f"}, run: func(dir string, argv []string, term Terminal) error {
		got = term
		return nil
	}}
	in, out, errw := strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}
	e.SetStdin(in)
	e.SetStdout(out)
	e.SetStderr(errw)
	if err := e.Run(); err != nil {
		t.Fatal(err)
	}
	if got.In != io.Reader(in) || got.Out != io.Writer(out) || got.Err != io.Writer(errw) {
		t.Fatalf("terminal %+v", got)
	}
}

func TestEditorArgv(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want []string
	}{
		{map[string]string{"VISUAL": "code --wait", "EDITOR": "nano"}, []string{"code", "--wait", "f"}},
		{map[string]string{"VISUAL": "", "EDITOR": "nano"}, []string{"nano", "f"}},
		{map[string]string{"VISUAL": "  ", "EDITOR": ""}, []string{"vi", "f"}},
		{nil, []string{"vi", "f"}},
	}
	for _, tc := range cases {
		if got := EditorArgv(func(k string) string { return tc.env[k] }, "f"); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("env %v: got %q, want %q", tc.env, got, tc.want)
		}
	}
}

// TestFormsAgainstRealBroker is the agent's stand-in for trying the forms by
// hand: a secret added through the form reaches a real broker, and the file
// keeps its comments.
func TestFormsAgainstRealBroker(t *testing.T) {
	file := "# t1: the real-broker test\nsecrets:\n  api-key: # the first key\n    command: [\"printf\", \"real-api-key-1\"]\n"
	cfg, cl := realBroker(t, file)
	h := newHarness(t, cl, Options{ConfigDir: cfg, Namespace: "t1"})
	h.keys("a")
	h.typeText("new-key")
	h.keys("tab")
	h.typeText("printf real-api-key-2")
	h.keys("enter")
	h.hasNot("Add a secret")
	h.has("saved secret new-key")
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		nss, err := cl.Status("t1")
		if err == nil && len(nss) == 1 && hasSecret(nss[0], "new-key") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the broker has no new-key: %+v %v", nss, err)
		}
	}
	data := read(t, filepath.Join(cfg, "t1.yaml"))
	for _, c := range []string{"# t1: the real-broker test", "# the first key"} {
		if !strings.Contains(data, c) {
			t.Fatalf("the file lost %q:\n%s", c, data)
		}
	}
}

func hasSecret(ns admin.NamespaceStatus, name string) bool {
	for _, s := range ns.Secrets {
		if s.Name == name {
			return true
		}
	}
	return false
}
