//go:build launchd

package launchd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abrose/sbx-broker/internal/admin"
	"github.com/abrose/sbx-broker/internal/secrets"
)

// testLabel is the only label the proof gives the real launchctl: it never
// touches the owner's agent io.github.abrose.sbx-broker.
const testLabel = "io.github.abrose.sbx-broker.test"

// The broker runs as a real launchd agent and its fetch command is found
// only through the PATH that the plist carries.
func TestLaunchdAgentFetchesWithCapturedPath(t *testing.T) {
	tmp := t.TempDir()
	prog := filepath.Join(tmp, "sbx-broker")
	if out, err := exec.Command("go", "build", "-o", prog, "github.com/abrose/sbx-broker/cmd/sbx-broker").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	bin := filepath.Join(tmp, "bin")
	mkdir(t, bin, 0o700)
	write(t, filepath.Join(bin, "fake-fetch"), "#!/bin/sh\nprintf %s real-api-key-1\n", 0o700)
	cfg := filepath.Join(tmp, "namespaces")
	mkdir(t, cfg, 0o700)
	write(t, filepath.Join(cfg, "t1.yaml"), "secrets:\n  api-key:\n    command: [\"fake-fetch\"]\n", 0o600)
	state, err := os.MkdirTemp("/tmp", "sbxb") // a socket path has at most 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	a := Agent{
		Label:     testLabel,
		PlistPath: filepath.Join(tmp, "LaunchAgents", testLabel+".plist"),
		Program:   prog,
		Env:       map[string]string{"PATH": bin + ":/usr/bin:/bin", "SBX_BROKER_CONFIG_DIR": cfg, "SBX_BROKER_STATE_DIR": state},
		LogPath:   filepath.Join(state, "serve.log"),
	}

	// A test agent left by an interrupted run goes first. The cleanup is in
	// place before the bootstrap, so a failing test boots the agent out too.
	if _, err := Uninstall(a, Real); err != nil {
		t.Fatalf("remove a leftover test agent: %v", err)
	}
	t.Cleanup(func() {
		if _, err := Uninstall(a, Real); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	if err := Install(a, Real); err != nil {
		t.Fatal(err)
	}
	waitOK(t, a, state)
	first := agentPID(t)

	// A second install with a changed env replaces the running agent: a new
	// process with the new env answers, the secret ok again.
	a.Env["SBX_BROKER_FETCH_TIMEOUT"] = "30s"
	if err := Install(a, Real); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	waitOK(t, a, state)
	if second := agentPID(t); second == first {
		t.Fatalf("the reinstalled agent runs the old process %d", first)
	}
	if out, _ := Real("print", target(testLabel)); !strings.Contains(string(out), "SBX_BROKER_FETCH_TIMEOUT => 30s") {
		t.Fatalf("the reinstalled agent lacks the new env:\n%s", out)
	}

	removed, err := Uninstall(a, Real)
	if err != nil || !removed {
		t.Fatalf("uninstall: removed %v, err %v", removed, err)
	}
	if loaded, err := Loaded(Real, testLabel); err != nil || loaded {
		t.Fatalf("launchctl print %s: loaded %v, err %v", target(testLabel), loaded, err)
	}
	if _, err := os.Stat(a.PlistPath); !os.IsNotExist(err) {
		t.Fatalf("the plist is still there: %v", err)
	}
	// bootout waits for the process; the poll only allows for a slow exit.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		lock, err := admin.TakeLock(state)
		if err == nil {
			_ = lock.Release()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a process still holds the lock of %s: %v", state, err)
		}
	}
}

// waitOK waits up to 75 s for the secret api-key of t1 to be ok.
func waitOK(t *testing.T, a Agent, state string) {
	t.Helper()
	deadline := time.Now().Add(75 * time.Second)
	for {
		st, err := secretState(state, "t1", "api-key")
		if err == nil && st == secrets.StateOK {
			return
		}
		if time.Now().After(deadline) {
			log, _ := os.ReadFile(a.LogPath)
			t.Fatalf("the secret is not ok after 75 s: state %q, err %v\nserve.log:\n%s", st, err, log)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// agentPID is the pid of the test agent once it is stable: two reads 200 ms
// apart give the same pid, within 10 s. A single read could catch the process
// of the old agent while it exits, or a process that KeepAlive restarts.
// The case of an old agent that lingers after bootout is proven only by the
// unit test TestInstallWaitsForBootout, which fakes the slow teardown.
func agentPID(t *testing.T) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	last := 0
	for {
		pid, ok, err := PID(Real, testLabel)
		if err != nil {
			t.Fatalf("launchctl print: %v", err)
		}
		if ok && pid == last {
			return pid
		}
		if !ok {
			pid = 0
		}
		last = pid
		if time.Now().After(deadline) {
			t.Fatalf("the agent has no stable pid after 10 s (last read %d)", pid)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// secretState asks the broker in state for the state of one secret.
func secretState(state, ns, name string) (secrets.State, error) {
	cl, err := admin.Dial(state)
	if err != nil {
		return "", err
	}
	var got secrets.State
	err = cl.Do(admin.Request{Op: "status", Namespace: ns}, func(r admin.Response) error {
		for _, n := range r.Namespaces {
			for _, s := range n.Secrets {
				if s.Name == name {
					got = s.State
				}
			}
		}
		return nil
	})
	if err == nil && got == "" {
		err = fmt.Errorf("no secret %s in namespace %s", name, ns)
	}
	return got, err
}

func mkdir(t *testing.T, dir string, mode os.FileMode) {
	t.Helper()
	if err := os.Mkdir(dir, mode); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}
