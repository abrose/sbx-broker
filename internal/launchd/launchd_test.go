package launchd

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
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
			return []byte("Could not find service"), &ExitError{Code: 113, Output: "Could not find service"}
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

// parsed is the plist as parsePlist reads it.
type parsed struct {
	Label                string
	ProgramArguments     []string
	EnvironmentVariables map[string]string
	RunAtLoad            bool
	KeepAlive            map[string]bool
	StandardOutPath      string
	StandardErrorPath    string
}

// lintPlist runs plutil -lint on macOS, as an extra check beside parsePlist.
func lintPlist(t *testing.T, plist []byte) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		return
	}
	path := filepath.Join(t.TempDir(), "a.plist")
	if err := os.WriteFile(path, plist, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("plutil", "-lint", path).CombinedOutput(); err != nil {
		t.Fatalf("plutil -lint: %v: %s\n%s", err, out, plist)
	}
}

// parsePlist reads the plist with encoding/xml, on every OS. It knows the
// elements the agent's plist uses: dict, array, string, true and false.
func parsePlist(t *testing.T, plist []byte) parsed {
	t.Helper()
	dec := xml.NewDecoder(bytes.NewReader(plist))
	root := nextStart(t, dec)
	if root.Name.Local != "plist" {
		t.Fatalf("the root is %s, not plist", root.Name.Local)
	}
	top, ok := plistValue(t, dec, nextStart(t, dec)).(map[string]any)
	if !ok {
		t.Fatal("the plist holds no dict")
	}
	p := parsed{
		Label:             plistGet[string](t, top, "Label"),
		RunAtLoad:         plistGet[bool](t, top, "RunAtLoad"),
		StandardOutPath:   plistGet[string](t, top, "StandardOutPath"),
		StandardErrorPath: plistGet[string](t, top, "StandardErrorPath"),
		KeepAlive:         map[string]bool{},
	}
	for _, v := range plistGet[[]any](t, top, "ProgramArguments") {
		p.ProgramArguments = append(p.ProgramArguments, v.(string))
	}
	p.EnvironmentVariables = map[string]string{}
	for k, v := range plistGet[map[string]any](t, top, "EnvironmentVariables") {
		p.EnvironmentVariables[k] = v.(string)
	}
	for k, v := range plistGet[map[string]any](t, top, "KeepAlive") {
		p.KeepAlive[k] = v.(bool)
	}
	return p
}

func plistGet[T any](t *testing.T, m map[string]any, k string) T {
	t.Helper()
	v, ok := m[k].(T)
	if !ok {
		t.Fatalf("key %s is %T, not %T", k, m[k], v)
	}
	return v
}

// nextStart returns the next start element, skipping text and directives.
func nextStart(t *testing.T, dec *xml.Decoder) xml.StartElement {
	t.Helper()
	for {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("xml: %v", err)
		}
		if s, ok := tok.(xml.StartElement); ok {
			return s
		}
	}
}

// plistValue reads the value that starts at start: a map, a slice, a string
// or a bool.
func plistValue(t *testing.T, dec *xml.Decoder, start xml.StartElement) any {
	t.Helper()
	switch start.Name.Local {
	case "true", "false":
		if err := dec.Skip(); err != nil {
			t.Fatalf("xml: %v", err)
		}
		return start.Name.Local == "true"
	case "string", "key":
		var s string
		if err := dec.DecodeElement(&s, &start); err != nil {
			t.Fatalf("xml: %v", err)
		}
		return s
	case "array":
		arr := []any{}
		for {
			tok, err := dec.Token()
			if err != nil {
				t.Fatalf("xml: %v", err)
			}
			switch e := tok.(type) {
			case xml.StartElement:
				arr = append(arr, plistValue(t, dec, e))
			case xml.EndElement:
				return arr
			}
		}
	case "dict":
		m := map[string]any{}
		for {
			tok, err := dec.Token()
			if err != nil {
				t.Fatalf("xml: %v", err)
			}
			switch e := tok.(type) {
			case xml.StartElement:
				if e.Name.Local != "key" {
					t.Fatalf("a dict has %s where a key belongs", e.Name.Local)
				}
				k := plistValue(t, dec, e).(string)
				m[k] = plistValue(t, dec, nextStart(t, dec))
			case xml.EndElement:
				return m
			}
		}
	}
	t.Fatalf("the plist has an unknown element %s", start.Name.Local)
	return nil
}

// The plist parses on every OS and holds the label, the program, the env,
// the keep-alive rule and the log paths.
func TestPlistStructure(t *testing.T) {
	a := agent(t)
	lintPlist(t, a.Plist())
	p := parsePlist(t, a.Plist())
	if p.Label != a.Label || !slices.Equal(p.ProgramArguments, []string{a.Program, "serve"}) {
		t.Fatalf("label %q, program arguments %q", p.Label, p.ProgramArguments)
	}
	if !maps.Equal(p.EnvironmentVariables, a.Env) {
		t.Fatalf("env %q", p.EnvironmentVariables)
	}
	if exit, ok := p.KeepAlive["SuccessfulExit"]; !p.RunAtLoad || len(p.KeepAlive) != 1 || !ok || exit {
		t.Fatalf("run at load %v, keep alive %v", p.RunAtLoad, p.KeepAlive)
	}
	if p.StandardOutPath != a.LogPath || p.StandardErrorPath != a.LogPath {
		t.Fatalf("out %q, err %q", p.StandardOutPath, p.StandardErrorPath)
	}
}

func TestPlistCarriesPathAndDirs(t *testing.T) {
	a := agent(t)
	plist := a.Plist()
	for _, want := range []string{"<string>/opt/homebrew/bin:/usr/bin:/bin</string>", "<string>/tmp/x</string>"} {
		if !strings.Contains(string(plist), want) {
			t.Fatalf("the plist has no %s:\n%s", want, plist)
		}
	}
	p := parsePlist(t, plist)
	if p.EnvironmentVariables["PATH"] != a.Env["PATH"] || p.EnvironmentVariables["SBX_BROKER_STATE_DIR"] != "/tmp/x" {
		t.Fatalf("env %q", p.EnvironmentVariables)
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
	lintPlist(t, a.Plist())
	if p := parsePlist(t, a.Plist()); p.EnvironmentVariables["PATH"] != a.Env["PATH"] {
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
			want = []string{"print", "bootout", "print", "bootstrap"}
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
	f := &fake{loaded: true}
	lc := func(args ...string) ([]byte, error) {
		calls = append(calls, args)
		return f.run(args...)
	}
	if err := Install(a, lc); err != nil {
		t.Fatal(err)
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	svc := domain + "/" + a.Label
	want := [][]string{{"print", svc}, {"bootout", svc}, {"print", svc}, {"bootstrap", domain, a.PlistPath}}
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
	if want := []string{"print", "bootout", "print", "bootstrap"}; !slices.Equal(f.calls, want) {
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

// lingering is a launchctl whose agent stays loaded for the first n prints
// after bootout, as launchd does while it tears the old service down.
type lingering struct {
	fake
	n int
}

func (l *lingering) run(args ...string) ([]byte, error) {
	if args[0] == "print" && !l.loaded && l.n > 0 {
		l.n--
		l.calls = append(l.calls, "print")
		return nil, nil
	}
	return l.fake.run(args...)
}

func TestInstallWaitsForBootout(t *testing.T) {
	a := agent(t)
	l := &lingering{fake: fake{loaded: true}, n: 3}
	var slept int
	if err := install(a, l.run, func(time.Duration) { slept++ }); err != nil {
		t.Fatal(err)
	}
	want := []string{"print", "bootout", "print", "print", "print", "print", "bootstrap"}
	if !slices.Equal(l.calls, want) {
		t.Fatalf("calls %q, want %q", l.calls, want)
	}
	if slept != 3 {
		t.Fatalf("slept %d times, want 3", slept)
	}
}

func TestInstallStopsWhenBootoutHangs(t *testing.T) {
	a := agent(t)
	lc := func(args ...string) ([]byte, error) {
		if args[0] == "bootstrap" {
			t.Fatal("bootstrap while the old agent is still loaded")
		}
		return nil, nil // print succeeds: loaded, always
	}
	var slept time.Duration
	err := install(a, lc, func(d time.Duration) { slept += d })
	if err == nil || !strings.Contains(err.Error(), a.Label) || !strings.Contains(err.Error(), "still loaded") {
		t.Fatalf("err %v", err)
	}
	if slept < 9*time.Second || slept > 11*time.Second {
		t.Fatalf("waited %v, want about 10 s", slept)
	}
}

func TestInstallBootstrapFails(t *testing.T) {
	a := agent(t)
	lc := func(args ...string) ([]byte, error) {
		switch args[0] {
		case "print":
			return nil, &ExitError{Code: 113}
		case "bootstrap":
			return []byte("Bootstrap failed: 5: Input/output error\n"), &ExitError{Code: 5}
		}
		return nil, nil
	}
	err := Install(a, lc)
	if err == nil || !strings.Contains(err.Error(), "Bootstrap failed: 5: Input/output error") || !strings.Contains(err.Error(), "launchctl bootstrap") {
		t.Fatalf("err %v", err)
	}
	if want := "the plist stays at " + a.PlistPath + ", sbx-broker uninstall removes it"; !strings.Contains(err.Error(), want) {
		t.Fatalf("err %v lacks %q", err, want)
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
	for _, loaded := range []bool{false, true} {
		got, err := Loaded((&fake{loaded: loaded}).run, "x")
		if err != nil || got != loaded {
			t.Fatalf("loaded %v: got %v, err %v", loaded, got, err)
		}
	}
}

// printFails is a launchctl whose print fails with err.
func printFails(err error) Launchctl {
	return func(args ...string) ([]byte, error) {
		if args[0] == "print" {
			return []byte("launchctl said no"), err
		}
		return nil, nil
	}
}

// Only exit 113 (service not found) means not loaded; any other failure is
// an error, not a silent "not loaded".
func TestLoadedOnlyExit113MeansNotLoaded(t *testing.T) {
	got, err := Loaded(printFails(&ExitError{Code: 113}), "x")
	if got || err != nil {
		t.Fatalf("exit 113: got %v, err %v", got, err)
	}
	for _, e := range []error{&ExitError{Code: 5}, errors.New("fork/exec /bin/launchctl: no such file")} {
		got, err := Loaded(printFails(e), "x")
		if got || err == nil || !errors.Is(err, e) {
			t.Fatalf("%v: got %v, err %v", e, got, err)
		}
	}
}

// Install, Uninstall and the wait for the unload return a print failure.
func TestPrintErrorReachesCallers(t *testing.T) {
	broken := printFails(&ExitError{Code: 5})
	a := agent(t)
	stays := "; the plist stays at " + a.PlistPath
	if err := Install(a, broken); err == nil || !strings.Contains(err.Error(), "launchctl print") ||
		!strings.HasSuffix(err.Error(), stays+", sbx-broker uninstall removes it") {
		t.Fatalf("install: %v", err)
	}
	if _, err := Uninstall(a, broken); err == nil || !strings.Contains(err.Error(), "launchctl print") ||
		!strings.HasSuffix(err.Error(), stays+", remove it by hand") {
		t.Fatalf("uninstall: %v", err)
	}
	// print works first, then fails while the install waits for the unload
	n := 0
	lc := func(args ...string) ([]byte, error) {
		if args[0] == "print" {
			if n++; n > 1 {
				return nil, &ExitError{Code: 5}
			}
		}
		return nil, nil
	}
	if err := install(a, lc, func(time.Duration) {}); err == nil || !strings.Contains(err.Error(), "launchctl print") ||
		!strings.HasSuffix(err.Error(), stays+", sbx-broker uninstall removes it") {
		t.Fatalf("wait: %v", err)
	}
}

func TestPID(t *testing.T) {
	print := func(out string, err error) Launchctl {
		return func(args ...string) ([]byte, error) { return []byte(out), err }
	}
	pid, ok, err := PID(print("x = {\n\tstate = running\n\tpid = 4711\n}\n", nil), "x")
	if pid != 4711 || !ok || err != nil {
		t.Fatalf("running: %d %v %v", pid, ok, err)
	}
	pid, ok, err = PID(print("x = {\n\tstate = not running\n}\n", nil), "x")
	if pid != 0 || ok || err != nil {
		t.Fatalf("loaded, no process: %d %v %v", pid, ok, err)
	}
	if _, ok, err = PID(print("", &ExitError{Code: 113}), "x"); ok || err == nil {
		t.Fatalf("not loaded: %v %v", ok, err)
	}
}
