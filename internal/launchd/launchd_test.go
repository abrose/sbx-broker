package launchd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// fake is a launchctl that knows one agent: print succeeds while it is
// loaded, bootstrap loads it, bootout unloads it.
type fake struct {
	loaded bool
	calls  []string
}

func (f *fake) run(args ...string) ([]byte, error) {
	f.calls = append(f.calls, args[0])
	switch args[0] {
	case "print":
		if !f.loaded {
			return []byte("Could not find service"), errors.New("exit status 113")
		}
	case "bootstrap":
		f.loaded = true
	case "bootout":
		f.loaded = false
	}
	return nil, nil
}

func agent(t *testing.T) Agent {
	t.Helper()
	dir := t.TempDir()
	return Agent{
		Label:     "io.github.abrose.sbx-broker.test",
		PlistPath: filepath.Join(dir, "LaunchAgents", "io.github.abrose.sbx-broker.test.plist"),
		Program:   "/opt/sbx/bin/sbx-broker",
		Env:       map[string]string{"PATH": "/opt/homebrew/bin:/usr/bin:/bin", "SBX_BROKER_STATE_DIR": "/tmp/x"},
		LogPath:   filepath.Join(dir, "state", "serve.log"),
	}
}

// parsed is the plist as plutil converts it to JSON.
type parsed struct {
	Label                string
	ProgramArguments     []string
	EnvironmentVariables map[string]string
	RunAtLoad            bool
	KeepAlive            map[string]bool
	StandardOutPath      string
	StandardErrorPath    string
}

// plutil lints the plist and converts it to JSON; it skips the test where
// plutil does not exist.
func plutil(t *testing.T, plist []byte) parsed {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("plutil runs on macOS only")
	}
	path := filepath.Join(t.TempDir(), "a.plist")
	if err := os.WriteFile(path, plist, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("plutil", "-lint", path).CombinedOutput(); err != nil {
		t.Fatalf("plutil -lint: %v: %s\n%s", err, out, plist)
	}
	out, err := exec.Command("plutil", "-convert", "json", "-o", "-", path).Output()
	if err != nil {
		t.Fatalf("plutil -convert: %v", err)
	}
	var p parsed
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return p
}

func TestPlistCarriesPathAndDirs(t *testing.T) {
	a := agent(t)
	plist := a.Plist()
	for _, want := range []string{"<string>/opt/homebrew/bin:/usr/bin:/bin</string>", "<string>/tmp/x</string>"} {
		if !strings.Contains(string(plist), want) {
			t.Fatalf("the plist has no %s:\n%s", want, plist)
		}
	}
	p := plutil(t, plist)
	if p.Label != a.Label || !slices.Equal(p.ProgramArguments, []string{a.Program, "serve"}) {
		t.Fatalf("label %q, program arguments %q", p.Label, p.ProgramArguments)
	}
	if len(p.EnvironmentVariables) != 2 || p.EnvironmentVariables["PATH"] != a.Env["PATH"] ||
		p.EnvironmentVariables["SBX_BROKER_STATE_DIR"] != "/tmp/x" {
		t.Fatalf("env %q", p.EnvironmentVariables)
	}
	if exit, ok := p.KeepAlive["SuccessfulExit"]; !p.RunAtLoad || len(p.KeepAlive) != 1 || !ok || exit {
		t.Fatalf("run at load %v, keep alive %v", p.RunAtLoad, p.KeepAlive)
	}
	if p.StandardOutPath != a.LogPath || p.StandardErrorPath != a.LogPath {
		t.Fatalf("out %q, err %q", p.StandardOutPath, p.StandardErrorPath)
	}
}

// The keys of the plist and of its env are sorted, so the same agent gives
// the same bytes.
func TestPlistKeysSorted(t *testing.T) {
	a := agent(t)
	a.Env["A_FIRST"] = "1"
	plist := string(a.Plist())
	order := []string{"EnvironmentVariables", "A_FIRST", "PATH", "SBX_BROKER_STATE_DIR", "KeepAlive", "SuccessfulExit",
		"Label", "ProgramArguments", "RunAtLoad", "StandardErrorPath", "StandardOutPath"}
	at := -1
	for _, k := range order {
		i := strings.Index(plist, "<key>"+k+"</key>")
		if i < at {
			t.Fatalf("%s is out of order:\n%s", k, plist)
		}
		at = i
	}
	if string(a.Plist()) != plist {
		t.Fatal("two renders differ")
	}
}

func TestPlistEscapesXML(t *testing.T) {
	a := agent(t)
	a.Env["PATH"] = "/opt/a&b/bin:/opt/<c>/bin:/usr/bin"
	plist := string(a.Plist())
	if strings.Contains(plist, "a&b") || strings.Contains(plist, "<c>") {
		t.Fatalf("not escaped:\n%s", plist)
	}
	if p := plutil(t, a.Plist()); p.EnvironmentVariables["PATH"] != a.Env["PATH"] {
		t.Fatalf("PATH %q", p.EnvironmentVariables["PATH"])
	}
}

func TestInstallCallsLaunchctlInOrder(t *testing.T) {
	for _, loaded := range []bool{true, false} {
		a := agent(t)
		f := &fake{loaded: loaded}
		if err := Install(a, f.run); err != nil {
			t.Fatal(err)
		}
		want := []string{"print", "bootstrap"}
		if loaded {
			want = []string{"print", "bootout", "bootstrap"}
		}
		if !slices.Equal(f.calls, want) {
			t.Fatalf("loaded %v: calls %q, want %q", loaded, f.calls, want)
		}
		got, err := os.ReadFile(a.PlistPath)
		if err != nil || string(got) != string(a.Plist()) {
			t.Fatalf("plist %q, %v", got, err)
		}
		if fi, err := os.Stat(a.PlistPath); err != nil || fi.Mode().Perm() != 0o644 {
			t.Fatalf("plist mode %v, %v", fi.Mode(), err)
		}
		if fi, err := os.Stat(filepath.Dir(a.LogPath)); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
			t.Fatalf("log dir %v, %v", fi, err)
		}
	}
}

// Install passes the domain, the service target and the plist path.
func TestInstallLaunchctlArguments(t *testing.T) {
	a := agent(t)
	var calls [][]string
	lc := func(args ...string) ([]byte, error) {
		calls = append(calls, args)
		return nil, nil // print succeeds: loaded
	}
	if err := Install(a, lc); err != nil {
		t.Fatal(err)
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	want := [][]string{{"print", domain + "/" + a.Label}, {"bootout", domain + "/" + a.Label}, {"bootstrap", domain, a.PlistPath}}
	if !slices.EqualFunc(calls, want, slices.Equal) {
		t.Fatalf("calls %q, want %q", calls, want)
	}
}

func TestInstallTwiceReplaces(t *testing.T) {
	a := agent(t)
	f := &fake{}
	if err := Install(a, f.run); err != nil {
		t.Fatal(err)
	}
	a.Env["PATH"] = "/usr/bin:/bin"
	f.calls = nil
	if err := Install(a, f.run); err != nil {
		t.Fatal(err)
	}
	if want := []string{"print", "bootout", "bootstrap"}; !slices.Equal(f.calls, want) {
		t.Fatalf("calls %q, want %q", f.calls, want)
	}
	got, err := os.ReadFile(a.PlistPath)
	if err != nil || string(got) != string(a.Plist()) {
		t.Fatalf("plist %q, %v", got, err)
	}
	entries, err := os.ReadDir(filepath.Dir(a.PlistPath))
	if err != nil || len(entries) != 1 {
		t.Fatalf("plist dir %v, %v: a temp file is left", entries, err)
	}
}

func TestInstallBootstrapFails(t *testing.T) {
	a := agent(t)
	lc := func(args ...string) ([]byte, error) {
		switch args[0] {
		case "print":
			return nil, errors.New("exit status 113")
		case "bootstrap":
			return []byte("Bootstrap failed: 5: Input/output error\n"), errors.New("exit status 5")
		}
		return nil, nil
	}
	err := Install(a, lc)
	if err == nil || !strings.Contains(err.Error(), "Bootstrap failed: 5: Input/output error") || !strings.Contains(err.Error(), "launchctl bootstrap") {
		t.Fatalf("err %v", err)
	}
}

func TestUninstallNotInstalled(t *testing.T) {
	a := agent(t)
	f := &fake{}
	removed, err := Uninstall(a, f.run)
	if err != nil || removed {
		t.Fatalf("removed %v, err %v", removed, err)
	}
	if !slices.Equal(f.calls, []string{"print"}) {
		t.Fatalf("calls %q", f.calls)
	}
}

func TestUninstallRemoves(t *testing.T) {
	a := agent(t)
	f := &fake{}
	if err := Install(a, f.run); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	removed, err := Uninstall(a, f.run)
	if err != nil || !removed {
		t.Fatalf("removed %v, err %v", removed, err)
	}
	if want := []string{"print", "bootout"}; !slices.Equal(f.calls, want) {
		t.Fatalf("calls %q, want %q", f.calls, want)
	}
	if _, err := os.Stat(a.PlistPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the plist is still there: %v", err)
	}
}

// A plist with no loaded agent is still removed, and so is a loaded agent
// with no plist.
func TestUninstallHalfInstalled(t *testing.T) {
	a := agent(t)
	if err := os.MkdirAll(filepath.Dir(a.PlistPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.PlistPath, a.Plist(), 0o644); err != nil {
		t.Fatal(err)
	}
	if removed, err := Uninstall(a, (&fake{}).run); err != nil || !removed {
		t.Fatalf("plist only: removed %v, err %v", removed, err)
	}
	f := &fake{loaded: true}
	if removed, err := Uninstall(a, f.run); err != nil || !removed || f.loaded {
		t.Fatalf("agent only: removed %v, err %v, loaded %v", removed, err, f.loaded)
	}
}

func TestLoaded(t *testing.T) {
	if Loaded((&fake{}).run, "x") || !Loaded((&fake{loaded: true}).run, "x") {
		t.Fatal("Loaded does not follow print")
	}
}
