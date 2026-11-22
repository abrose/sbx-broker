package harness

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const earlyExitChild = "SBXB_HARNESS_EARLY_EXIT_CHILD"

// TestEarlyExitChild runs only in the child process of TestBrokerEarlyExit:
// it serves a broker that stops at once, so it fails on purpose.
func TestEarlyExitChild(t *testing.T) {
	if os.Getenv(earlyExitChild) != "1" {
		t.Skip("runs only as the child of TestBrokerEarlyExit")
	}
	serve(t, func(context.Context) error { return errors.New("config dir: boom") },
		func() string { return "not ready yet" })
}

// TestBrokerEarlyExit: a broker that stops before it is ready fails the test
// at once, with one error that names the cause, and the cleanup does not
// wait for it to stop again.
func TestBrokerEarlyExit(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestEarlyExitChild$", "-test.count=1")
	cmd.Env = append(os.Environ(), earlyExitChild+"=1")
	start := time.Now()
	out, err := cmd.CombinedOutput()
	took := time.Since(start)
	text := string(out)
	if err == nil {
		t.Fatalf("the child passed:\n%s", text)
	}
	if took > 5*time.Second {
		t.Errorf("the child took %v, want under 5 s", took.Round(time.Millisecond))
	}
	if !strings.Contains(text, "the broker stopped: config dir: boom") {
		t.Errorf("no error names the cause:\n%s", text)
	}
	if n := strings.Count(text, "boom"); n != 1 {
		t.Errorf("the cause is reported %d times, want once:\n%s", n, text)
	}
	if strings.Contains(text, "did not stop") {
		t.Errorf("the cleanup reports a broker that did not stop:\n%s", text)
	}
}
