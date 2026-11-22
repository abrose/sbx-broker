// Command sbx-broker swaps credential placeholders of sandboxed apps for the
// real values. `serve` runs the broker; the other commands ask it over its
// admin socket.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/abrose/sbx-broker/internal/admin"
	"github.com/abrose/sbx-broker/internal/eventlog"
	"github.com/abrose/sbx-broker/internal/secrets"
	"github.com/abrose/sbx-broker/internal/server"
)

const usage = `usage: sbx-broker <command>

commands:
  serve                      run the broker until SIGINT or SIGTERM
  status [<ns>]              namespaces, listeners and secret states, without values
  refresh <ns> [<secret>]    run the fetch commands again
  reload                     read the namespace files again now
  log [-f] [<ns>]            the last 100 events; -f follows new ones
  help                       this text

environment:
  SBX_BROKER_CONFIG_DIR      the namespace files (default ~/.config/sbx-broker/namespaces)
  SBX_BROKER_STATE_DIR       admin.sock and broker.log (default ~/.local/state/sbx-broker)
  SBX_BROKER_FETCH_TIMEOUT   the timeout of a fetch command, a Go duration (default 60s)
`

const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
	noBroker  = "no broker runs (start it with: sbx-broker serve)"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// run runs one command and returns the exit code.
func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	c := &command{getenv: getenv, stdout: stdout, stderr: stderr}
	cmd, rest := args[0], args[1:]
	switch {
	case cmd == "help" || cmd == "-h" || cmd == "--help":
		fmt.Fprint(stdout, usage)
		return exitOK
	case cmd == "serve" && len(rest) == 0:
		return c.serve()
	case cmd == "status" && len(rest) <= 1:
		return c.status(rest)
	case cmd == "refresh" && len(rest) >= 1 && len(rest) <= 2:
		return c.refresh(rest)
	case cmd == "reload" && len(rest) == 0:
		return c.reload()
	case cmd == "log":
		if follow, ns, ok := logArgs(rest); ok {
			return c.log(follow, ns)
		}
	}
	fmt.Fprint(stderr, usage)
	return exitUsage
}

func logArgs(args []string) (follow bool, ns string, ok bool) {
	for _, a := range args {
		switch {
		case a == "-f":
			follow = true
		case strings.HasPrefix(a, "-") || ns != "":
			return false, "", false
		default:
			ns = a
		}
	}
	return follow, ns, true
}

// command runs one CLI command with its environment and output.
type command struct {
	getenv         func(string) string
	stdout, stderr io.Writer
}

func (c *command) fail(err error) int {
	if errors.Is(err, admin.ErrNoBroker) {
		fmt.Fprintln(c.stderr, noBroker)
	} else {
		fmt.Fprintf(c.stderr, "sbx-broker: %v\n", err)
	}
	return exitFail
}

// dir returns the env value of key, or the default under the home dir.
func (c *command) dir(key string, def ...string) (string, error) {
	if v := c.getenv(key); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("%s is not set and %w", key, err)
	}
	return filepath.Join(append([]string{home}, def...)...), nil
}

func (c *command) stateDir() (string, error) {
	return c.dir("SBX_BROKER_STATE_DIR", ".local", "state", "sbx-broker")
}

func (c *command) serve() int {
	cfg, err := c.dir("SBX_BROKER_CONFIG_DIR", ".config", "sbx-broker", "namespaces")
	if err != nil {
		return c.fail(err)
	}
	state, err := c.stateDir()
	if err != nil {
		return c.fail(err)
	}
	opt := server.Options{ConfigDir: cfg, StateDir: state}
	if v := c.getenv("SBX_BROKER_FETCH_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return c.fail(fmt.Errorf("SBX_BROKER_FETCH_TIMEOUT %q is not a positive Go duration", v))
		}
		opt.FetchTimeout = d
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := server.Run(ctx, opt); err != nil {
		return c.fail(err)
	}
	return exitOK
}

// do sends one request to the broker.
func (c *command) do(req admin.Request, each func(admin.Response) error) error {
	state, err := c.stateDir()
	if err != nil {
		return err
	}
	cl, err := admin.Dial(state)
	if err != nil {
		return err
	}
	return cl.Do(req, each)
}

// namespaces sends req and returns the namespaces of its answer.
func (c *command) namespaces(req admin.Request) ([]admin.NamespaceStatus, error) {
	var out []admin.NamespaceStatus
	err := c.do(req, func(r admin.Response) error {
		out = append(out, r.Namespaces...)
		return nil
	})
	return out, err
}

func (c *command) status(args []string) int {
	req := admin.Request{Op: "status"}
	if len(args) == 1 {
		req.Namespace = args[0]
	}
	nss, err := c.namespaces(req)
	if err != nil {
		return c.fail(err)
	}
	if len(nss) == 0 {
		fmt.Fprintln(c.stdout, "no namespaces")
		return exitOK
	}
	printStatus(c.stdout, nss)
	return exitOK
}

// refresh exits 1 when a refreshed secret is not ok afterwards.
func (c *command) refresh(args []string) int {
	req := admin.Request{Op: "refresh", Namespace: args[0]}
	if len(args) == 2 {
		req.Secret = args[1]
	}
	nss, err := c.namespaces(req)
	if err != nil {
		return c.fail(err)
	}
	printStatus(c.stdout, nss)
	for _, ns := range nss {
		for _, s := range ns.Secrets {
			if (req.Secret == "" || s.Name == req.Secret) && s.State != secrets.StateOK {
				return exitFail
			}
		}
	}
	return exitOK
}

func (c *command) reload() int {
	if err := c.do(admin.Request{Op: "reload"}, func(admin.Response) error { return nil }); err != nil {
		return c.fail(err)
	}
	fmt.Fprintln(c.stdout, "reloaded")
	return exitOK
}

func (c *command) log(follow bool, ns string) int {
	err := c.do(admin.Request{Op: "log", Namespace: ns, Follow: follow}, func(r admin.Response) error {
		if r.Event != nil {
			_, err := fmt.Fprintln(c.stdout, logLine(*r.Event))
			return err
		}
		return nil
	})
	if err != nil {
		return c.fail(err)
	}
	return exitOK
}

// printStatus writes one header line per namespace and one aligned line
// per secret, sorted by name as the broker sends them.
func printStatus(w io.Writer, nss []admin.NamespaceStatus) {
	for _, ns := range nss {
		ok := 0
		for _, s := range ns.Secrets {
			if s.State == secrets.StateOK {
				ok++
			}
		}
		where := "no listeners"
		if len(ns.Listeners) > 0 {
			where = "listening on " + strings.Join(ns.Listeners, ", ")
		}
		fmt.Fprintf(w, "namespace %s: %d of %d secrets ok, %s\n", ns.Name, ok, len(ns.Secrets), where)
		var table bytes.Buffer
		tw := tabwriter.NewWriter(&table, 0, 0, 3, ' ', 0)
		for _, s := range ns.Secrets {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t", s.Name, s.State, s.Placeholder)
			switch s.State {
			case secrets.StateOK:
				fmt.Fprintf(tw, "len %d sha %s\tfetched %s\t", s.Length, s.Fingerprint, s.FetchedAt.Local().Format(time.TimeOnly))
			case secrets.StateFailed:
				fmt.Fprintf(tw, "%s\t", failure(s))
			}
			fmt.Fprintln(tw)
		}
		_ = tw.Flush() // a bytes.Buffer never fails
		for _, line := range strings.SplitAfter(table.String(), "\n") {
			if line != "" {
				fmt.Fprintln(w, strings.TrimRight(line, " \n"))
			}
		}
		if ns.Error != "" {
			fmt.Fprintf(w, "  error: %s\n", ns.Error)
		}
	}
}

// failure is the exit code and the first stderr line of a failed fetch.
func failure(s secrets.Status) string {
	if s.ExitCode > 0 {
		if s.Err == "" || s.Err == fmt.Sprintf("exit status %d", s.ExitCode) {
			return fmt.Sprintf("exit %d", s.ExitCode)
		}
		return fmt.Sprintf("exit %d: %s", s.ExitCode, s.Err)
	}
	return s.Err
}

// logLine is one event: time, kind, namespace, route, method, path,
// status, secret names, message. An empty field is "-".
func logLine(e eventlog.Event) string {
	status := ""
	if e.Status != 0 {
		status = strconv.Itoa(e.Status)
	}
	fields := []string{e.Time.Local().Format(time.TimeOnly), string(e.Kind), e.Namespace, e.Route, e.Method, e.Path,
		status, strings.Join(e.Secrets, ","), e.Message}
	for i, f := range fields {
		if f == "" {
			fields[i] = "-"
		}
	}
	return strings.Join(fields, " ")
}
