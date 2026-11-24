package nsfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/abrose/sbx-broker/internal/config"
)

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
	f, _ := open(t, Template)
	argv := []string{"op", "read", "op://<vault>/<item>/credential"}
	if err := f.SetSecret("api-key", argv); err != nil {
		t.Fatal(err)
	}
	out := bytesOf(t, f)
	pos := 0
	for _, line := range strings.Split(strings.TrimSuffix(Template, "\n"), "\n") {
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

func assertLinesInOrder(t *testing.T, out, text string) {
	t.Helper()
	pos := 0
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		i := strings.Index(out[pos:], line)
		if i < 0 {
			t.Fatalf("line %q missing or out of order in\n%s", line, out)
		}
		pos += i + len(line)
	}
}

func TestRemoveLastSecretKeepsTemplateHeadComment(t *testing.T) {
	f, _ := open(t, Template+"\nsecrets:\n  k:\n    command: [\"a\"]\n")
	must(t, f.RemoveSecret("k"))
	assertLinesInOrder(t, bytesOf(t, f), Template)
}

func TestRemoveFirstSectionKeepsFileHeader(t *testing.T) {
	header := "# file header line 1\n# file header line 2\n"
	f, _ := open(t, header+"secrets:\n  k:\n    command: [\"a\"]\n")
	must(t, f.RemoveSecret("k"))
	assertLinesInOrder(t, bytesOf(t, f), header)
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
	f, _ := open(t, Template)
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
	f, _ := open(t, Template)
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
	f, dir := open(t, Template)
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
	f, dir := open(t, Template)
	must(t, f.SetSecret("k", []string{"echo", "x"}))
	boom := errors.New("boom")
	if err := f.Save(func([]byte) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "acme.yaml")); string(got) != Template {
		t.Errorf("file changed: %q", got)
	}
	assertOnlyFile(t, dir, "acme.yaml")
}

func TestSaveWritesMode0600AtomicNoTmpLeft(t *testing.T) {
	f, dir := open(t, Template)
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

// A stale temp file (an earlier crash, or a symlink planted there) does not
// block a save, and a save never writes through it: each save has its own.
func TestStaleTmpDoesNotBlockSave(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		f, dir := open(t, Template)
		tmp := filepath.Join(dir, ".acme.yaml.tmp")
		outside := filepath.Join(t.TempDir(), "outside")
		must(t, os.WriteFile(outside, []byte("keep me"), 0o600))
		if symlink {
			must(t, os.Symlink(outside, tmp))
		} else {
			must(t, os.WriteFile(tmp, []byte("stale"), 0o600))
		}
		must(t, f.SetSecret("k", []string{"echo", "x"}))
		if err := f.Save(func([]byte) error { return nil }); err != nil {
			t.Fatalf("symlink=%v: %v", symlink, err)
		}
		if got := readFile(t, filepath.Join(dir, "acme.yaml")); !strings.Contains(got, "k:") {
			t.Errorf("symlink=%v: file = %q", symlink, got)
		}
		if got := readFile(t, outside); got != "keep me" {
			t.Errorf("symlink=%v: outside file = %q", symlink, got)
		}
		if fi, err := os.Lstat(tmp); err != nil || (fi.Mode()&os.ModeSymlink != 0) != symlink {
			t.Errorf("symlink=%v: the stale tmp did not stay: %v %v", symlink, fi, err)
		}
		entries, err := os.ReadDir(dir)
		must(t, err)
		if len(entries) != 2 {
			t.Errorf("symlink=%v: dir holds %d entries, want acme.yaml and .acme.yaml.tmp", symlink, len(entries))
		}
	}
}

func TestOpenRefusesSymlinkAndGroupWritable(t *testing.T) {
	dir := newDir(t)
	real := writeNS(t, dir, "real", Template)
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

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSaveRequiresCheck(t *testing.T) {
	f, dir := open(t, Template)
	must(t, f.SetSecret("k", []string{"echo", "x"}))
	if err := f.Save(nil); err == nil || err.Error() != "nsfile: Save needs a check" {
		t.Fatalf("err = %v", err)
	}
	if got := readFile(t, filepath.Join(dir, "acme.yaml")); got != Template {
		t.Errorf("file changed: %q", got)
	}
	assertOnlyFile(t, dir, "acme.yaml")
}

// A file removed, or swapped for a symlink, after Open is a changed file: the
// TUI reads it again. A save never writes through the symlink.
func TestSaveFileGoneOrSymlinkedIsErrChanged(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		f, dir := open(t, Template)
		p := filepath.Join(dir, "acme.yaml")
		must(t, os.Remove(p))
		target := filepath.Join(newDir(t), "target.yaml")
		if symlink {
			must(t, os.WriteFile(target, []byte("keep me"), 0o600))
			must(t, os.Symlink(target, p))
		}
		must(t, f.SetSecret("k", []string{"echo", "x"}))
		if err := f.Save(func([]byte) error { return nil }); !errors.Is(err, ErrChanged) {
			t.Errorf("symlink=%v: err = %v", symlink, err)
		}
		if symlink {
			if got := readFile(t, target); got != "keep me" {
				t.Errorf("symlink target = %q", got)
			}
		}
	}
}

func TestSaveRefusesGroupWritableDir(t *testing.T) {
	f, dir := open(t, Template)
	must(t, os.Chmod(dir, 0o770))
	must(t, f.SetSecret("k", []string{"echo", "x"}))
	err := f.Save(func([]byte) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "is writable by group or others") {
		t.Fatalf("err = %v", err)
	}
	if got := readFile(t, filepath.Join(dir, "acme.yaml")); got != Template {
		t.Errorf("file changed: %q", got)
	}
	assertOnlyFile(t, dir, "acme.yaml")
}

// Two saves of one file at the same time: each writes its own temp file, so
// the file is one complete output, never a mix, and no temp file stays.
func TestConcurrentSavesLeaveOneCompleteFile(t *testing.T) {
	_, dir := open(t, Template)
	p := filepath.Join(dir, "acme.yaml")
	var files [2]*File
	var outs [2]string
	for i := range files {
		f, err := Open(p)
		must(t, err)
		must(t, f.SetSecret(fmt.Sprintf("k%d", i), []string{"echo", "x"}))
		files[i], outs[i] = f, bytesOf(t, f)
	}
	inside, release := make(chan struct{}), make(chan struct{})
	check := func([]byte) error {
		inside <- struct{}{}
		<-release
		return nil
	}
	errs := make(chan error, len(files))
	for _, f := range files {
		go func() { errs <- f.Save(check) }()
	}
	for n := 0; n < len(files); {
		select {
		case <-inside:
			n++
		case err := <-errs:
			t.Fatalf("a save returned before both were inside the check: %v", err)
		}
	}
	close(release)
	failed := 0
	for range files {
		if err := <-errs; err != nil {
			failed++
			if !errors.Is(err, ErrChanged) {
				t.Errorf("err = %v", err)
			}
		}
	}
	if failed > 1 {
		t.Errorf("both saves failed")
	}
	if got := readFile(t, p); got != outs[0] && got != outs[1] {
		t.Errorf("file is neither output:\n%s", got)
	}
	assertOnlyFile(t, dir, "acme.yaml")
}

// The temp file of earlier versions (.acme.yaml.tmp) is not a namespace file
// and does not block a save.
func TestOldFixedTmpIsIgnored(t *testing.T) {
	f, dir := open(t, Template)
	must(t, os.WriteFile(filepath.Join(dir, ".acme.yaml.tmp"), []byte("junk: [\n"), 0o600))
	ok, bad, err := config.LoadDir(dir)
	if err != nil || len(bad) != 0 || len(ok) != 1 || ok["acme"] == nil {
		t.Fatalf("LoadDir: %v %v %v", ok, bad, err)
	}
	must(t, f.SetSecret("k", []string{"echo", "x"}))
	if err := f.Save(func([]byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

const tailComment = `secrets:
  a:
    command: ["x"]
  b:
    command: ["y"]
  # tail

http:
  - name: web
    listen: 9080
    upstream: https://api.example.com
`

// A comment under the last key of a section belongs to the key yaml.v3 hangs
// it on; removing that key or its section keeps the comment.
func TestRemoveLastKeepsFootComment(t *testing.T) {
	f, _ := open(t, tailComment)
	must(t, f.RemoveSecret("a"))
	if out := bytesOf(t, f); !strings.Contains(out, "# tail") {
		t.Errorf("tail lost after removing a\n%s", out)
	}
	must(t, f.RemoveSecret("b"))
	out := bytesOf(t, f)
	if !strings.Contains(out, "# tail") {
		t.Errorf("tail lost after removing the section\n%s", out)
	}
	load(t, out)

	g, _ := open(t, tailComment)
	must(t, g.RemoveSecret("b"))
	out = bytesOf(t, g)
	assertLinesInOrder(t, out, "  a:\n    command: [\"x\"]\n  # tail\n")
	load(t, out)

	// The foot comment of a first key moves to the key after it.
	h, _ := open(t, "secrets:\n  a:\n    command: [\"x\"]\n  # under a\n\n  b:\n    command: [\"y\"]\n")
	must(t, h.RemoveSecret("a"))
	out = bytesOf(t, h)
	assertLinesInOrder(t, out, "secrets:\n  # under a\n  b:\n")
	load(t, out)
}

func TestHas(t *testing.T) {
	f, _ := open(t, commented)
	for _, tc := range []struct {
		kind Kind
		name string
		want bool
	}{
		{KindSecret, "db-pass", true}, {KindSecret, "other", true}, {KindSecret, "api", false},
		{KindRoute, "api", true}, {KindRoute, "db-pass", false},
		{KindDatabase, "app", true}, {KindDatabase, "api", false},
	} {
		if got := f.Has(tc.kind, tc.name); got != tc.want {
			t.Errorf("Has(%v, %q) = %v, want %v", tc.kind, tc.name, got, tc.want)
		}
	}
	// It sees the edits in memory, and an empty file has nothing.
	must(t, f.RemoveRoute("api"))
	if f.Has(KindRoute, "api") {
		t.Error("a removed route is still there")
	}
	empty, _ := open(t, Template)
	if empty.Has(KindSecret, "api-key") || empty.Has(KindRoute, "api") || empty.Has(KindDatabase, "app") {
		t.Error("a file of comments has entries")
	}
}

// The getters give an entry's values as the file writes them (an edit form
// starts from them), with the edits made so far; a missing entry is
// ErrNotFound. Source is the bytes Open read.
func TestGetters(t *testing.T) {
	text := strings.Replace(commented, "      password: db-pass\n", "      password: db-pass\n      require-auth: scram-sha-256\n", 1)
	text = strings.Replace(text, "upstream: https://api.example.com", "upstream: https://API.example.com:443/", 1)
	f, _ := open(t, text)
	if string(f.Source()) != text {
		t.Fatalf("Source %q", f.Source())
	}
	if cmd, err := f.Secret("db-pass"); err != nil || !reflect.DeepEqual(cmd, []string{"echo", "x"}) {
		t.Errorf("Secret: %q %v", cmd, err)
	}
	if listen, up, err := f.Route("api"); err != nil || listen != "29710" || up != "https://API.example.com:443/" {
		t.Errorf("Route: %q %q %v", listen, up, err)
	}
	want := Database{Alias: "app", Upstream: "127.0.0.1:5432", DBName: "appdb", User: "appuser", Password: "db-pass", RequireAuth: "scram-sha-256"}
	if d, listen, err := f.Database("app"); err != nil || d != want || listen != "29720" {
		t.Errorf("Database: %+v %q %v", d, listen, err)
	}
	if got := f.PostgresListen(); got != "29720" {
		t.Errorf("PostgresListen %q", got)
	}

	must(t, f.SetSecret("other", []string{"printf", "a b"}))
	if cmd, err := f.Secret("other"); err != nil || !reflect.DeepEqual(cmd, []string{"printf", "a b"}) {
		t.Errorf("Secret after SetSecret: %q %v", cmd, err)
	}

	_, err := f.Secret("nope")
	_, _, rerr := f.Route("nope")
	_, _, derr := f.Database("nope")
	for _, err := range []error{err, rerr, derr} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("a missing entry: %v", err)
		}
	}
	empty, _ := open(t, Template)
	if _, err := empty.Secret("api-key"); !errors.Is(err, ErrNotFound) || empty.PostgresListen() != "" {
		t.Errorf("a file of comments: %v, listen %q", err, empty.PostgresListen())
	}

	// A command that is not a list of strings is an error, not an empty command.
	odd, _ := open(t, "secrets:\n  a:\n    command: printf x\n  b:\n    command: [[\"printf\"]]\n")
	for _, name := range []string{"a", "b"} {
		if cmd, err := odd.Secret(name); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("Secret(%q): %q %v", name, cmd, err)
		}
	}
}
