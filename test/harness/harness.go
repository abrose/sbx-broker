// Package harness runs the pieces of the integration proof: the recorder
// mock in a container (testcontainers-go) and the broker in this process,
// with the proof namespaces t1 and t2 on the fixed test ports.
package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/abrose/sbx-broker/internal/admin"
	"github.com/abrose/sbx-broker/internal/placeholder"
	"github.com/abrose/sbx-broker/internal/secrets"
	"github.com/abrose/sbx-broker/internal/server"
	"github.com/abrose/sbx-broker/test/cases"
)

// The proof values. They are test values, not secrets.
const (
	ValueAPIKey       = "real-api-key-1"
	ValueClientSecret = "real-client-secret-1"
	ValueOldSecret    = "real-old-secret-9"
	ValuePass         = "p@ss word/1+&"
	ValueOtherAPIKey  = "other-api-key-2"
)

// The fixed test ports of the proof routes.
const (
	PortT1API = 29691
	PortT1IdP = 29692
	PortT2API = 29693
)

const (
	casesPort   = "8080/tcp"
	recordsPort = "8081/tcp"
	startWait   = 10 * time.Second
	stopWait    = 15 * time.Second
)

// ProofValues maps a label to each proof value: the secret name in t1, and
// t2/api-key for the value of t2.
func ProofValues() map[string]string {
	return map[string]string{
		"api-key":       ValueAPIKey,
		"client-secret": ValueClientSecret,
		"old-secret":    ValueOldSecret,
		"pass":          ValuePass,
		"t2/api-key":    ValueOtherAPIKey,
	}
}

// ProofNamespaces returns the files t1 and t2, with both routes of t1 and
// the route of t2 to the recorder at recorderBase.
func ProofNamespaces(recorderBase string) map[string]string {
	t1 := fmt.Sprintf(`secrets:
  api-key:
    command: ["printf", "%%s", %q]
  client-secret:
    command: ["printf", "%%s", %q]
  old-secret:
    command: ["printf", "%%s", %q]
  pass:
    command: ["printf", "%%s", %q]
  broken:
    command: ["false"]
http:
  - name: api
    listen: %d
    upstream: %s
  - name: idp
    listen: %d
    upstream: %s
`, ValueAPIKey, ValueClientSecret, ValueOldSecret, ValuePass, PortT1API, recorderBase, PortT1IdP, recorderBase)
	t2 := fmt.Sprintf(`secrets:
  api-key:
    command: ["printf", "%%s", %q]
http:
  - name: api
    listen: %d
    upstream: %s
`, ValueOtherAPIKey, PortT2API, recorderBase)
	return map[string]string{"t1": t1, "t2": t2}
}

// ProofRoutes maps the route names of the cases to the routes of t1.
func ProofRoutes() map[string]string {
	return map[string]string{
		cases.RouteAPI: fmt.Sprintf("http://127.0.0.1:%d", PortT1API),
		cases.RouteIdP: fmt.Sprintf("http://127.0.0.1:%d", PortT1IdP),
	}
}

// ProofPlaceholders maps the keys of the cases to the placeholders.
func ProofPlaceholders() map[string]string {
	return map[string]string{
		cases.APIKey:       placeholder.Format("t1", "api-key"),
		cases.ClientSecret: placeholder.Format("t1", "client-secret"),
		cases.OldSecret:    placeholder.Format("t1", "old-secret"),
		cases.Pass:         placeholder.Format("t1", "pass"),
		cases.Broken:       placeholder.Format("t1", "broken"),
		cases.Undefined:    placeholder.Format("t1", "nope"),
		cases.OtherNS:      placeholder.Format("t2", "api-key"),
	}
}

// StartRecorder builds the recorder image, runs it with
// MOCK_CLIENT_SECRET set to the client secret of t1, and terminates it when
// the test ends. Both ports are published on 127.0.0.1 only. It returns the
// base URL of the cases and of the records.
func StartRecorder(t *testing.T) (base, records string) {
	t.Helper()
	ctx := context.Background()
	ctr, err := testcontainers.Run(ctx, "",
		testcontainers.WithDockerfile(testcontainers.FromDockerfile{Context: recorderDir(t)}),
		testcontainers.WithEnv(map[string]string{"MOCK_CLIENT_SECRET": ValueClientSecret}),
		testcontainers.WithExposedPorts(casesPort, recordsPort),
		testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
			loopback := []network.PortBinding{{HostIP: netip.MustParseAddr("127.0.0.1")}}
			hc.PortBindings = network.PortMap{
				network.MustParsePort(casesPort):   loopback,
				network.MustParsePort(recordsPort): loopback,
			}
		}),
		// Each port has its own server: wait for both. The cases port gets
		// no request, since every request there is recorded.
		testcontainers.WithWaitStrategy(wait.ForAll(
			wait.ForHTTP("/records").WithPort(recordsPort).WithForcedIPv4LocalHost(),
			wait.ForListeningPort(casesPort),
		)),
	)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start the recorder: %v", err)
	}
	return loopbackURL(t, ctr, casesPort), loopbackURL(t, ctr, recordsPort)
}

func loopbackURL(t *testing.T, ctr *testcontainers.DockerContainer, port string) string {
	t.Helper()
	p, err := ctr.MappedPort(context.Background(), port)
	if err != nil {
		t.Fatalf("mapped port %s: %v", port, err)
	}
	return "http://127.0.0.1:" + p.Port()
}

// recorderDir is test/mocks/recorder, found from this file.
func recorderDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot find the harness source")
	}
	return filepath.Join(filepath.Dir(file), "..", "mocks", "recorder")
}

// Record is one request the recorder got.
type Record struct {
	Method   string      `json:"method"`
	Path     string      `json:"path"`    // decoded
	RawPath  string      `json:"rawPath"` // escaped, as the request line had it
	RawQuery string      `json:"rawQuery"`
	Headers  http.Header `json:"headers"`
	Body     string      `json:"body"`
}

// Records returns every request the recorder got since the last clear.
func Records(t *testing.T, records string) []Record {
	t.Helper()
	resp, err := http.Get(records + "/records")
	if err != nil {
		t.Fatalf("get the records: %v", err)
	}
	defer resp.Body.Close()
	var out []Record
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode the records: %v", err)
	}
	return out
}

// ClearRecords deletes the recorder's records.
func ClearRecords(t *testing.T, records string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, records+"/records", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("clear the records: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("clear the records: status %d", resp.StatusCode)
	}
}

// StartBroker writes the namespace files (name to content) as serve demands
// them (an owner-only dir and files, no symlink), runs the broker until the
// test ends, and waits until every namespace runs all its listeners and no
// secret is pending. The state dir is short, under /tmp (ruling R5).
func StartBroker(t *testing.T, namespaces map[string]string) (configDir, stateDir string) {
	t.Helper()
	configDir = filepath.Join(t.TempDir(), "namespaces")
	if err := os.Mkdir(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range namespaces {
		p := filepath.Join(configDir, name+".yaml")
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o600); err != nil { // the umask may have cut more
			t.Fatal(err)
		}
	}
	stateDir, err := os.MkdirTemp("/tmp", "sbxb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })

	serve(t, func(ctx context.Context) error {
		return server.Run(ctx, server.Options{ConfigDir: configDir, StateDir: stateDir, FetchTimeout: startWait})
	}, func() string { return notReady(stateDir, len(namespaces)) })
	return configDir, stateDir
}

// brokerRun is a broker that runs in the background.
type brokerRun struct {
	stopped  chan struct{} // closed when the run returned
	err      error         // what the run returned; read it after stopped
	reported bool          // the test already failed with err
}

// serve runs run until the test ends, and waits until ready returns "". A
// run that stops before that fails the test at once, and the cleanup does
// not wait for it or report it again.
func serve(t *testing.T, run func(context.Context) error, ready func() string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	b := &brokerRun{stopped: make(chan struct{})}
	go func() {
		b.err = run(ctx)
		close(b.stopped)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-b.stopped:
			if b.err != nil && !b.reported {
				t.Errorf("the broker: %v", b.err)
			}
		case <-time.After(stopWait):
			t.Error("the broker did not stop")
		}
	})
	waitReady(t, ready, b)
}

// waitReady polls ready until it returns "".
func waitReady(t *testing.T, ready func() string, b *brokerRun) {
	t.Helper()
	deadline := time.Now().Add(startWait)
	last := "no answer"
	for time.Now().Before(deadline) {
		select {
		case <-b.stopped:
			b.reported = true
			t.Fatalf("the broker stopped: %v", b.err)
		default:
		}
		var why string
		if why = ready(); why == "" {
			return
		}
		last = why
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the broker is not ready: %s", last)
}

// notReady says why the broker is not ready, or "": it is ready when n
// namespaces run with no listener error and no pending secret.
func notReady(stateDir string, n int) string {
	c, err := admin.Dial(stateDir)
	if err != nil {
		return err.Error()
	}
	var nss []admin.NamespaceStatus
	if err := c.Do(admin.Request{Op: "status"}, func(r admin.Response) error {
		nss = r.Namespaces
		return nil
	}); err != nil {
		return err.Error()
	}
	if len(nss) != n {
		return fmt.Sprintf("%d of %d namespaces run", len(nss), n)
	}
	for _, ns := range nss {
		if ns.Error != "" {
			return "namespace " + ns.Name + ": " + ns.Error
		}
		var pending []string
		for _, s := range ns.Secrets {
			if s.State == secrets.StatePending {
				pending = append(pending, s.Name)
			}
		}
		if len(pending) > 0 {
			return "namespace " + ns.Name + ": pending " + strings.Join(pending, ", ")
		}
	}
	return ""
}
