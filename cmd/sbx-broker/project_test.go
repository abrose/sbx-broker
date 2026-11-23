package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/abrose/sbx-broker/internal/config"
)

// editRun is one CLI run with a recording editor.
type editRun struct {
	code        int
	out, errOut string
	argv        []string // nil when no editor ran
	dir         string   // the editor's working folder
}

func cliEdit(dir string, env map[string]string, args ...string) editRun {
	var out, errOut bytes.Buffer
	var r editRun
	c := &command{
		getenv:    func(k string) string { return env[k] },
		getwd:     func() (string, error) { return dir, nil },
		runEditor: func(dir string, argv []string) error { r.dir, r.argv = dir, argv; return nil },
		stdout:    &out,
		stderr:    &errOut,
	}
	r.code = c.run(args)
	r.out, r.errOut = out.String(), errOut.String()
	return r
}

// configEnv returns an env whose config folder is cfg (not yet created).
func configEnv(t *testing.T) (map[string]string, string) {
	cfg := filepath.Join(t.TempDir(), "namespaces")
	return map[string]string{"SBX_BROKER_CONFIG_DIR": cfg}, cfg
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
		if got := editorArgv(func(k string) string { return tc.env[k] }, "f"); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("env %v: got %q, want %q", tc.env, got, tc.want)
		}
	}
}

func TestEditUsesMarker(t *testing.T) {
	env, cfg := configEnv(t)
	writeFile(t, filepath.Join(cfg, "acme.yaml"), "", 0o600)
	r := cliEdit(project(t, "acme"), env, "edit")
	if want := []string{"vi", filepath.Join(cfg, "acme.yaml")}; r.code != 0 || !reflect.DeepEqual(r.argv, want) {
		t.Fatalf("code %d, argv %q, err %q", r.code, r.argv, r.errOut)
	}
}

// The editor must not run in the project folder: the agent can write it, and
// some editors load config (and run commands) from their working folder.
func TestEditRunsInConfigFolder(t *testing.T) {
	env, cfg := configEnv(t)
	writeFile(t, filepath.Join(cfg, "acme.yaml"), "", 0o600)
	r := cliEdit(project(t, "acme"), env, "edit")
	if r.code != 0 || r.dir != cfg {
		t.Fatalf("code %d, editor folder %q, want %q", r.code, r.dir, cfg)
	}
}

func TestEditArgument(t *testing.T) {
	env, cfg := configEnv(t)
	writeFile(t, filepath.Join(cfg, "acme.yaml"), "", 0o600)
	r := cliEdit(noProject, env, "edit", "acme")
	if want := []string{"vi", filepath.Join(cfg, "acme.yaml")}; r.code != 0 || !reflect.DeepEqual(r.argv, want) {
		t.Fatalf("code %d, argv %q, err %q", r.code, r.argv, r.errOut)
	}
}

func TestEditMissingFile(t *testing.T) {
	env, _ := configEnv(t)
	r := cliEdit(noProject, env, "edit", "acme")
	if r.code != 1 || !strings.Contains(r.errOut, "sbx-broker init acme") || r.argv != nil {
		t.Fatalf("code %d, argv %q, err %q", r.code, r.argv, r.errOut)
	}
}

func TestEditNoMarker(t *testing.T) {
	env, _ := configEnv(t)
	r := cliEdit(noProject, env, "edit")
	if r.code != 2 || !strings.Contains(r.errOut, "no .sbx-broker marker") || r.argv != nil {
		t.Fatalf("code %d, err %q", r.code, r.errOut)
	}
}

func TestEditBadName(t *testing.T) {
	env, _ := configEnv(t)
	r := cliEdit(noProject, env, "edit", "../x")
	if r.code != 2 || !strings.Contains(r.errOut, "not a valid namespace name") || r.argv != nil {
		t.Fatalf("code %d, err %q", r.code, r.errOut)
	}
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestInitNewProject(t *testing.T) {
	env, cfg := configEnv(t)
	dir := t.TempDir()
	r := cliEdit(dir, env, "init", "acme")
	if r.code != 0 || !strings.Contains(r.out, "next: sbx-broker edit") {
		t.Fatalf("code %d, out %q, err %q", r.code, r.out, r.errOut)
	}
	marker := filepath.Join(dir, markerName)
	if b, _ := os.ReadFile(marker); string(b) != "acme\n" || mode(t, marker) != 0o644 {
		t.Fatalf("marker %q mode %o", b, mode(t, marker))
	}
	if mode(t, cfg) != 0o700 {
		t.Fatalf("config folder mode %o", mode(t, cfg))
	}
	file := filepath.Join(cfg, "acme.yaml")
	if b, _ := os.ReadFile(file); string(b) != namespaceTemplate || mode(t, file) != 0o600 {
		t.Fatalf("namespace file mode %o, template %v", mode(t, file), string(b) == namespaceTemplate)
	}
}

func TestInitKeepsNamespaceFile(t *testing.T) {
	env, cfg := configEnv(t)
	file := filepath.Join(cfg, "acme.yaml")
	writeFile(t, file, "x: 1\n", 0o600)
	r := cliEdit(t.TempDir(), env, "init", "acme")
	if b, _ := os.ReadFile(file); r.code != 0 || string(b) != "x: 1\n" || !strings.Contains(r.out, "kept "+file) {
		t.Fatalf("code %d, file %q, out %q", r.code, b, r.out)
	}
}

func TestInitSameMarker(t *testing.T) {
	env, _ := configEnv(t)
	dir := project(t, "acme")
	r := cliEdit(dir, env, "init", "acme")
	if b, _ := os.ReadFile(filepath.Join(dir, markerName)); r.code != 0 || string(b) != "acme\n" {
		t.Fatalf("code %d, marker %q, err %q", r.code, b, r.errOut)
	}
}

func TestInitOtherMarker(t *testing.T) {
	env, cfg := configEnv(t)
	r := cliEdit(project(t, "zzz"), env, "init", "acme")
	if r.code != 1 || !strings.Contains(r.errOut, "names another namespace") || strings.Contains(r.errOut, "zzz") {
		t.Fatalf("code %d, err %q", r.code, r.errOut)
	}
	if _, err := os.Stat(filepath.Join(cfg, "acme.yaml")); !os.IsNotExist(err) {
		t.Fatalf("a namespace file was created: %v", err)
	}
}

func TestInitSymlinkMarker(t *testing.T) {
	env, _ := configEnv(t)
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "key")
	writeFile(t, target, "s3cr3t", 0o600)
	if err := os.Symlink(target, filepath.Join(dir, markerName)); err != nil {
		t.Fatal(err)
	}
	r := cliEdit(dir, env, "init", "acme")
	if r.code != 1 || strings.Contains(r.out+r.errOut, "s3cr3t") {
		t.Fatalf("code %d, out %q, err %q", r.code, r.out, r.errOut)
	}
	if b, _ := os.ReadFile(target); string(b) != "s3cr3t" {
		t.Fatalf("the target changed: %q", b)
	}
}

func TestInitBadName(t *testing.T) {
	env, _ := configEnv(t)
	dir := t.TempDir()
	r := cliEdit(dir, env, "init", "../x")
	if _, err := os.Stat(filepath.Join(dir, markerName)); r.code != 2 || !os.IsNotExist(err) {
		t.Fatalf("code %d, marker: %v", r.code, err)
	}
}

// escapeDir returns a folder whose name holds a terminal escape, as an agent
// can create one in the workspace.
func escapeDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "evil\x1b]0;x\x07")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestInitEscapesPaths(t *testing.T) {
	env, _ := configEnv(t)
	r := cliEdit(escapeDir(t), env, "init", "acme")
	if r.code != 0 || strings.ContainsAny(r.out+r.errOut, "\x1b\x07") {
		t.Fatalf("code %d, out %q, err %q", r.code, r.out, r.errOut)
	}
}

func TestMarkerErrorEscapesPath(t *testing.T) {
	env, _ := configEnv(t)
	dir := escapeDir(t)
	writeFile(t, filepath.Join(dir, markerName), "../x", 0o644)
	r := cliEdit(dir, env, "edit")
	if r.code != 1 || strings.ContainsAny(r.errOut, "\x1b\x07") {
		t.Fatalf("code %d, err %q", r.code, r.errOut)
	}
}

func TestTemplateLoads(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "namespaces")
	writeFile(t, filepath.Join(dir, "acme.yaml"), namespaceTemplate, 0o600)
	ok, bad, err := config.LoadDir(dir)
	if err != nil || len(bad) != 0 || ok["acme"] == nil || len(ok["acme"].Secrets) != 0 {
		t.Fatalf("ok %v, bad %v, err %v", ok, bad, err)
	}
}
