package nsfile

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/abrose/sbx-broker/internal/config"
)

// template is the text of `sbx-broker init` (cmd/sbx-broker namespaceTemplate).
const template = `# An sbx-broker namespace: the secrets and services of one project.
# The broker reads this file again within about 2 s of a change.
# Reference: https://github.com/abrose/sbx-broker/blob/main/docs/configuration.md
#
# secrets:
#   api-key:                  # the placeholder is sbxb_<namespace>_api_key (each - as _)
#     command: ["op", "read", "op://<vault>/<item>/credential"]
#
# http:
#   - name: api
#     listen: 29610           # the broker's port for this route, 1024 to 65535
#     upstream: https://<host>
`

const commented = `# head comment
# second head line

# postgres block comment
postgres:
  listen: 29720 # pg port
  databases:
    app: # app database
      upstream: 127.0.0.1:5432
      dbname: appdb
      user: appuser
      password: db-pass
# http block comment
http:
  - name: api # api route
    listen: 29710
    upstream: https://api.example.com
# secrets block comment
secrets:
  db-pass: # the keeper
    command: ["echo", "x"]
  other:
    command: ["echo", "y"]
`

func newDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeNS(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name+".yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func open(t *testing.T, content string) (*File, string) {
	t.Helper()
	dir := newDir(t)
	p := writeNS(t, dir, "acme", content)
	f, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	return f, dir
}

func bytesOf(t *testing.T, f *File) string {
	t.Helper()
	b, err := f.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// load parses text as the namespace acme through the config package.
func load(t *testing.T, text string) *config.Namespace {
	t.Helper()
	dir := newDir(t)
	writeNS(t, dir, "acme", text)
	ok, bad, err := config.LoadDir(dir)
	if err != nil || len(bad) != 0 {
		t.Fatalf("load: %v %v\n%s", err, bad, text)
	}
	return ok["acme"]
}

func topKeys(t *testing.T, text string) []string {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(text), &n); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for i := 0; i < len(n.Content[0].Content); i += 2 {
		keys = append(keys, n.Content[0].Content[i].Value)
	}
	return keys
}

func TestSetSecretOnCommentOnlyFile(t *testing.T) {
	f, _ := open(t, template)
	argv := []string{"op", "read", "op://<vault>/<item>/credential"}
	if err := f.SetSecret("api-key", argv); err != nil {
		t.Fatal(err)
	}
	out := bytesOf(t, f)
	pos := 0
	for _, line := range strings.Split(strings.TrimSuffix(template, "\n"), "\n") {
		i := strings.Index(out[pos:], line)
		if i < 0 {
			t.Fatalf("comment line %q missing or out of order in\n%s", line, out)
		}
		pos += i + len(line)
	}
	ns := load(t, out)
	if len(ns.Secrets) != 1 || ns.Secrets[0].Name != "api-key" || !reflect.DeepEqual(ns.Secrets[0].Command, argv) {
		t.Fatalf("secrets = %+v", ns.Secrets)
	}
}

func TestEditsKeepCommentsAndKeyOrder(t *testing.T) {
	f, _ := open(t, commented)
	must(t, f.SetSecret("fresh", []string{"echo", "z"}))
	must(t, f.SetRoute(Route{Name: "api", Listen: 29711, Upstream: "https://api.example.com"}))
	must(t, f.SetDatabase(29720, Database{Alias: "app", Upstream: "127.0.0.1:5432", DBName: "appdb", User: "other", Password: "db-pass"}))
	must(t, f.RemoveSecret("other"))
	out := bytesOf(t, f)
	for _, c := range []string{
		"# head comment", "# second head line", "# postgres block comment", "# pg port",
		"# app database", "# http block comment", "# api route", "# secrets block comment", "# the keeper",
	} {
		if !strings.Contains(out, c) {
			t.Errorf("comment %q lost in\n%s", c, out)
		}
	}
	if got := topKeys(t, out); !reflect.DeepEqual(got, []string{"postgres", "http", "secrets"}) {
		t.Errorf("key order = %v", got)
	}
	ns := load(t, out)
	if ns.HTTP[0].Listen != 29711 || ns.Postgres.Databases[0].User != "other" || len(ns.Secrets) != 2 {
		t.Errorf("loaded = %+v %+v", ns.HTTP, ns.Postgres)
	}
	if !strings.Contains(out, "  db-pass:") || strings.Contains(out, "other:\n    command") {
		t.Errorf("unexpected text\n%s", out)
	}
}

func TestSetSecretChangesCommandOfExisting(t *testing.T) {
	f, _ := open(t, commented)
	must(t, f.SetSecret("db-pass", []string{"gopass", "show", "-o", "<path>"}))
	out := bytesOf(t, f)
	if !strings.Contains(out, "# the keeper") {
		t.Errorf("comment lost\n%s", out)
	}
	for _, s := range load(t, out).Secrets {
		if s.Name == "db-pass" && !reflect.DeepEqual(s.Command, []string{"gopass", "show", "-o", "<path>"}) {
			t.Errorf("command = %v", s.Command)
		}
	}
}

func TestSetRouteKeepsTransforms(t *testing.T) {
	f, _ := open(t, "http:\n  - name: api\n    listen: 29710\n    upstream: https://a.example.com\n    transforms: [swap]\n")
	must(t, f.SetRoute(Route{Name: "api", Listen: 29710, Upstream: "https://b.example.com"}))
	out := bytesOf(t, f)
	if !strings.Contains(out, "transforms: [swap]") {
		t.Errorf("transforms changed\n%s", out)
	}
	if got := load(t, out).HTTP[0].Upstream.String(); got != "https://b.example.com" {
		t.Errorf("upstream = %s", got)
	}
}

func TestSetRouteAddsRoute(t *testing.T) {
	f, _ := open(t, template)
	must(t, f.SetRoute(Route{Name: "idp", Listen: 29610, Upstream: "https://idp.example.com"}))
	must(t, f.SetRoute(Route{Name: "web", Listen: 29611, Upstream: "http://127.0.0.1:9080"}))
	ns := load(t, bytesOf(t, f))
	if len(ns.HTTP) != 2 || ns.HTTP[1].Name != "web" || ns.HTTP[1].Listen != 29611 {
		t.Errorf("http = %+v", ns.HTTP)
	}
	must(t, f.RemoveRoute("idp"))
	if ns := load(t, bytesOf(t, f)); len(ns.HTTP) != 1 || ns.HTTP[0].Name != "web" {
		t.Errorf("http = %+v", ns.HTTP)
	}
}

func TestSetDatabaseRequireAuth(t *testing.T) {
	f, _ := open(t, "secrets:\n  pw:\n    command: [\"echo\", \"x\"]\n")
	db := Database{Alias: "app", Upstream: "127.0.0.1:5432", DBName: "appdb", User: "u", Password: "pw", RequireAuth: "scram-sha-256"}
	must(t, f.SetDatabase(29720, db))
	out := bytesOf(t, f)
	if !strings.Contains(out, "require-auth: scram-sha-256") {
		t.Errorf("key missing\n%s", out)
	}
	if got := load(t, out).Postgres.Databases[0]; got.RequireAuth != "scram-sha-256" || got.User != "u" {
		t.Errorf("db = %+v", got)
	}
	db.RequireAuth = ""
	must(t, f.SetDatabase(29720, db))
	out = bytesOf(t, f)
	if strings.Contains(out, "require-auth") {
		t.Errorf("key kept\n%s", out)
	}
	load(t, out)
}

func TestRemoveLastDatabaseRemovesPostgres(t *testing.T) {
	f, _ := open(t, commented)
	must(t, f.RemoveDatabase("app"))
	out := bytesOf(t, f)
	if strings.Contains(out, "postgres:") {
		t.Errorf("postgres kept\n%s", out)
	}
	if ns := load(t, out); ns.Postgres != nil {
		t.Errorf("postgres = %+v", ns.Postgres)
	}
}

func TestRemoveUnknownIsErrNotFound(t *testing.T) {
	f, _ := open(t, template)
	for name, err := range map[string]error{
		"secret":   f.RemoveSecret("nope"),
		"route":    f.RemoveRoute("nope"),
		"database": f.RemoveDatabase("nope"),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	g, _ := open(t, commented)
	if err := g.RemoveSecret("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v", err)
	}
	if err := g.RemoveRoute("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v", err)
	}
	if err := g.RemoveDatabase("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestSaveRefusesChangedFile(t *testing.T) {
	f, dir := open(t, template)
	outside := "# written from outside\n"
	writeNS(t, dir, "acme", outside)
	must(t, f.SetSecret("k", []string{"echo", "x"}))
	err := f.Save(func([]byte) error { return nil })
	if !errors.Is(err, ErrChanged) {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "acme.yaml")); string(got) != outside {
		t.Errorf("file = %q", got)
	}
}

func TestSaveRunsCheckFirst(t *testing.T) {
	f, dir := open(t, template)
	must(t, f.SetSecret("k", []string{"echo", "x"}))
	boom := errors.New("boom")
	if err := f.Save(func([]byte) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "acme.yaml")); string(got) != template {
		t.Errorf("file changed: %q", got)
	}
	assertOnlyFile(t, dir, "acme.yaml")
}

func TestSaveWritesMode0600AtomicNoTmpLeft(t *testing.T) {
	f, dir := open(t, template)
	must(t, f.SetSecret("k", []string{"echo", "x"}))
	var seen []byte
	err := f.Save(func(b []byte) error {
		seen = b
		return config.Check(dir, "acme.yaml", b)
	})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "acme.yaml")
	got, _ := os.ReadFile(p)
	if string(got) != string(seen) || !strings.Contains(string(got), "k:") {
		t.Errorf("file = %q, checked = %q", got, seen)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode())
	}
	assertOnlyFile(t, dir, "acme.yaml")
	// A second save from the same File works: it knows what it wrote.
	must(t, f.SetSecret("k2", []string{"echo", "y"}))
	if err := f.Save(func([]byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRefusesSymlinkAndGroupWritable(t *testing.T) {
	dir := newDir(t)
	real := writeNS(t, dir, "real", template)
	link := filepath.Join(dir, "link.yaml")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(link); err == nil {
		t.Error("symlink accepted")
	}
	if err := os.Chmod(real, 0o660); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(real); err == nil {
		t.Error("group-writable file accepted")
	}
	if _, err := Open(dir); err == nil {
		t.Error("directory accepted")
	}
}

func assertOnlyFile(t *testing.T, dir, name string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != name {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dir holds %v, want only %s", names, name)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
