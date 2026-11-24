package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const goodYAML = `
secrets:
  idp-client-secret:
    command: ["gopass", "show", "-o", "<path>"]
  orders-api-key:
    command: ["op", "read", "op://<vault>/<item>/credential"]
  app-db-password:
    command: ["sh", "-c", "grep '^<host>:5432:' ~/.pgpass | cut -d: -f5"]

http:
  - name: idp
    listen: 29610
    upstream: https://idp.example.com
  - name: orders-api
    listen: 29611
    upstream: http://127.0.0.1:9080

postgres:
  listen: 29620
  databases:
    app:
      upstream: 127.0.0.1:5432
      dbname: appdb
      user: appuser
      password: app-db-password
`

func newDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeFile(t *testing.T, dir, file, content string) {
	t.Helper()
	p := filepath.Join(dir, file)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
}

const routeOn = `
http:
  - name: api
    listen: %d
    upstream: https://api.example.com
`

func TestCheckRefusesPortOfOtherNamespace(t *testing.T) {
	dir := newDir(t)
	writeFile(t, dir, "b.yaml", fmt.Sprintf(routeOn, 29700))
	err := Check(dir, "a.yaml", []byte(fmt.Sprintf(routeOn, 29700)))
	if err == nil || err.Error() != "port 29700 is used by namespace b" {
		t.Fatalf("err = %v", err)
	}
}

func TestCheckAcceptsOwnFileOnDisk(t *testing.T) {
	dir := newDir(t)
	writeFile(t, dir, "a.yaml", fmt.Sprintf(routeOn, 29700))
	if err := Check(dir, "a.yaml", []byte(fmt.Sprintf(routeOn, 29700))); err != nil {
		t.Fatal(err)
	}
}

func TestCheckIgnoresInvalidOtherFiles(t *testing.T) {
	dir := newDir(t)
	writeFile(t, dir, "b.yaml", fmt.Sprintf(routeOn, 29700)+"bogus: 1\n")
	if err := Check(dir, "a.yaml", []byte(fmt.Sprintf(routeOn, 29700))); err != nil {
		t.Fatal(err)
	}
}

func TestCheckAppliesLoadRules(t *testing.T) {
	dir := newDir(t)
	if err := Check(dir, "a.yaml", []byte("secrets:\n  k:\n    command: []\n")); err == nil {
		t.Fatal("empty command accepted")
	}
	if err := Check(dir, "A.yaml", []byte("")); err == nil {
		t.Fatal("invalid namespace name accepted")
	}
	if err := Check(dir, "a.yaml", []byte("nope: 1\n")); err == nil {
		t.Fatal("unknown key accepted")
	}
}

func TestLoadGood(t *testing.T) {
	dir := newDir(t)
	writeFile(t, dir, "acme.yaml", goodYAML)

	ok, bad, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 0 {
		t.Fatalf("bad: %v", bad)
	}
	ns := ok["acme"]
	if ns == nil {
		t.Fatalf("acme not loaded: %v", ok)
	}
	if ns.Name != "acme" || ns.Path != filepath.Join(dir, "acme.yaml") {
		t.Errorf("name/path: %q %q", ns.Name, ns.Path)
	}
	if len(ns.Secrets) != 3 {
		t.Errorf("secrets: %v", ns.Secrets)
	}
	for _, s := range ns.Secrets {
		if s.Name == "idp-client-secret" && !reflect.DeepEqual(s.Command, []string{"gopass", "show", "-o", "<path>"}) {
			t.Errorf("command: %v", s.Command)
		}
	}
	if len(ns.HTTP) != 2 {
		t.Fatalf("http: %v", ns.HTTP)
	}
	r := ns.HTTP[1]
	if r.Name != "orders-api" || r.Listen != 29611 || r.Upstream.String() != "http://127.0.0.1:9080" {
		t.Errorf("route: %+v", r)
	}
	want := []TransformSpec{{Name: "swap", Settings: map[string]string{}}}
	if !reflect.DeepEqual(r.Transforms, want) {
		t.Errorf("transforms: %v, want %v", r.Transforms, want)
	}
	if ns.Postgres == nil || ns.Postgres.Listen != 29620 || len(ns.Postgres.Databases) != 1 {
		t.Fatalf("postgres: %+v", ns.Postgres)
	}
	db := ns.Postgres.Databases[0]
	if db != (Database{Alias: "app", Upstream: "127.0.0.1:5432", DBName: "appdb", User: "appuser", Password: "app-db-password"}) {
		t.Errorf("db: %+v", db)
	}
	if got := ns.Ports(); !reflect.DeepEqual(got, []int{29610, 29611, 29620}) {
		t.Errorf("ports: %v", got)
	}
}

func TestLoadRequireAuth(t *testing.T) {
	pg := func(extra string) string {
		return "secrets:\n  pw:\n    command: [\"x\"]\npostgres:\n  listen: 29620\n  databases:\n    app:\n      upstream: 127.0.0.1:1\n      dbname: d\n      user: u\n      password: pw\n" + extra
	}
	load := func(content string) (map[string]*Namespace, map[string]error) {
		dir := newDir(t)
		writeFile(t, dir, "acme.yaml", content)
		ok, bad, err := LoadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		return ok, bad
	}
	for content, want := range map[string]string{pg(""): "", pg("      require-auth: scram-sha-256\n"): "scram-sha-256"} {
		ok, bad := load(content)
		if len(bad) != 0 || ok["acme"] == nil {
			t.Fatalf("bad: %v", bad)
		}
		if got := ok["acme"].Postgres.Databases[0].RequireAuth; got != want {
			t.Errorf("RequireAuth %q, want %q", got, want)
		}
	}
	for _, v := range []string{"md5", "SCRAM-SHA-256", "none", `""`, "", "~", "null"} {
		ok, bad := load(pg("      require-auth: " + v + "\n"))
		e := bad["acme.yaml"]
		if len(ok) != 0 || e == nil || !strings.Contains(e.Error(), `"app"`) || !strings.Contains(e.Error(), "require-auth") {
			t.Errorf("value %s: ok=%v err=%v", v, ok, e)
		}
	}
}

func TestLoadTransformForms(t *testing.T) {
	dir := newDir(t)
	writeFile(t, dir, "acme.yaml", `
http:
  - name: api
    listen: 29610
    upstream: http://127.0.0.1:9080
    transforms:
      - swap
`)
	ok, bad, err := LoadDir(dir)
	if err != nil || len(bad) != 0 {
		t.Fatalf("err=%v bad=%v", err, bad)
	}
	if got := ok["acme"].HTTP[0].Transforms; len(got) != 1 || got[0].Name != "swap" {
		t.Errorf("transforms: %v", got)
	}
}

func TestLoadBad(t *testing.T) {
	route := func(extra string) string {
		return "http:\n  - name: api\n    listen: 29610\n    upstream: http://127.0.0.1:9080\n" + extra
	}
	tests := []struct {
		name, file, content, want string
	}{
		{"namespace name", "a-b.yaml", route(""), "namespace name"},
		{"unknown key", "acme.yaml", "bogus: 1\n", "field"},
		{"unknown nested key", "acme.yaml", route("    bogus: 1\n"), "field"},
		{"secret name", "acme.yaml", "secrets:\n  A:\n    command: [\"x\"]\n", "secret name"},
		{"empty command", "acme.yaml", "secrets:\n  k:\n    command: []\n", "command"},
		{"duplicate route", "acme.yaml", route("  - name: api\n    listen: 29611\n    upstream: http://127.0.0.1:9080\n"), "route name"},
		{"low port", "acme.yaml", "http:\n  - name: api\n    listen: 80\n    upstream: http://127.0.0.1:9080\n", "port"},
		{"ftp upstream", "acme.yaml", "http:\n  - name: api\n    listen: 29610\n    upstream: ftp://x\n", "upstream"},
		{"upstream path", "acme.yaml", "http:\n  - name: api\n    listen: 29610\n    upstream: http://h/path\n", "upstream"},
		{"unknown transform", "acme.yaml", route("    transforms: [foo]\n"), "unknown transform"},
		{"undefined password secret", "acme.yaml", "postgres:\n  listen: 29620\n  databases:\n    app:\n      upstream: 127.0.0.1:1\n      dbname: d\n      user: u\n      password: nope\n", "password"},
		{"port twice", "acme.yaml", route("  - name: other\n    listen: 29610\n    upstream: http://127.0.0.1:9080\n"), "port 29610"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := newDir(t)
			writeFile(t, dir, tc.file, tc.content)
			ok, bad, err := LoadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(ok) != 0 {
				t.Errorf("ok should be empty: %v", ok)
			}
			e := bad[tc.file]
			if e == nil {
				t.Fatalf("no error for %s; bad=%v", tc.file, bad)
			}
			if !strings.Contains(e.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", e, tc.want)
			}
		})
	}
}

func TestLoadPortClashAcrossFiles(t *testing.T) {
	dir := newDir(t)
	c := "http:\n  - name: api\n    listen: 29610\n    upstream: http://127.0.0.1:9080\n"
	writeFile(t, dir, "a.yaml", c)
	writeFile(t, dir, "b.yaml", c)
	ok, bad, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ok["a"] == nil || ok["b"] != nil {
		t.Errorf("ok: %v", ok)
	}
	if e := bad["b.yaml"]; e == nil || !strings.Contains(e.Error(), "port 29610 is used by namespace a") {
		t.Errorf("bad: %v", bad)
	}
}

func TestLoadRefusesWritableFile(t *testing.T) {
	dir := newDir(t)
	writeFile(t, dir, "acme.yaml", goodYAML)
	if err := os.Chmod(filepath.Join(dir, "acme.yaml"), 0o664); err != nil {
		t.Fatal(err)
	}
	ok, bad, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ok) != 0 {
		t.Errorf("ok: %v", ok)
	}
	if e := bad["acme.yaml"]; e == nil || !strings.Contains(e.Error(), "writable by group or others") {
		t.Errorf("bad: %v", bad)
	}
}

func TestLoadRefusesWritableDir(t *testing.T) {
	dir := newDir(t)
	writeFile(t, dir, "acme.yaml", goodYAML)
	if err := os.Chmod(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	_, _, err := LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), "writable by group or others") {
		t.Errorf("err: %v", err)
	}
}

func TestLoadRefusesSymlinkDir(t *testing.T) {
	dir := newDir(t)
	link := filepath.Join(newDir(t), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadDir(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("err: %v", err)
	}
}

func TestLoadMissingDir(t *testing.T) {
	if _, _, err := LoadDir(filepath.Join(newDir(t), "nope")); err == nil {
		t.Error("want an error")
	}
}

func TestCheckOwnerOnlyInfo(t *testing.T) {
	dir := newDir(t)
	writeFile(t, dir, "own.yaml", "")
	writeFile(t, dir, "shared.yaml", "")
	if err := os.Chmod(filepath.Join(dir, "shared.yaml"), 0o660); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path, want string // want "" is no error
	}{
		{filepath.Join(dir, "own.yaml"), ""},
		{filepath.Join(dir, "shared.yaml"), "is writable by group or others (mode 0660)"},
		{dir, "is not a regular file"},
	} {
		fi, err := os.Lstat(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		err = CheckOwnerOnlyInfo(tc.path, fi, false)
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: err = %v, want %q", tc.path, err, tc.want)
		}
	}
}

func TestLoadRefusesSymlink(t *testing.T) {
	dir := newDir(t)
	writeFile(t, dir, "real.yaml", goodYAML)
	if err := os.Symlink(filepath.Join(dir, "real.yaml"), filepath.Join(dir, "c.yaml")); err != nil {
		t.Fatal(err)
	}
	ok, bad, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ok["c"] != nil {
		t.Errorf("c loaded: %v", ok)
	}
	if e := bad["c.yaml"]; e == nil || !strings.Contains(e.Error(), "symlink") {
		t.Errorf("bad: %v", bad)
	}
}
