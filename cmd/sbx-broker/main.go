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
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/abrose/sbx-broker/internal/admin"
	"github.com/abrose/sbx-broker/internal/eventlog"
	"github.com/abrose/sbx-broker/internal/launchd"
	"github.com/abrose/sbx-broker/internal/secrets"
	"github.com/abrose/sbx-broker/internal/server"
	"github.com/abrose/sbx-broker/internal/tui"
)

const usage = `usage: sbx-broker [<command>]

commands:
  tui                        the terminal UI, also with no command: namespaces,
                             secrets, a secret's details and a confirmed reveal
  serve                      run the broker until SIGINT or SIGTERM
  status [<ns>]              namespaces, listeners and secret states, without values
  refresh [<ns> [<secret>]]  run the fetch commands again
  reload                     read the namespace files again now and retry
                             the files and listeners that failed
  log [-f] [<ns>]            the last 100 events; -f follows new ones
  edit [<ns>]                open the namespace file in $VISUAL or $EDITOR
  init <ns>                  mark this folder as a project of <ns> and create its namespace file
  install                    run serve as a launchd agent: at login and again after a
                             crash, with the PATH and SBX_BROKER_* of this shell
  uninstall                  stop the launchd agent and remove it
  help                       this text

<ns> defaults to the name in a .sbx-broker file in this folder or above.

environment:
  SBX_BROKER_CONFIG_DIR      the namespace files (default ~/.config/sbx-broker/namespaces)
  SBX_BROKER_STATE_DIR       admin.sock, admin.lock, broker.log and the launchd agent's serve.log
                             (default ~/.local/state/sbx-broker)
  SBX_BROKER_FETCH_TIMEOUT   the timeout of a fetch command, a Go duration (default 60s)
`

const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
	noBroker  = tui.NoBroker
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// run runs one command and returns the exit code.
func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	c := &command{getenv: getenv, getwd: os.Getwd, home: os.UserHomeDir, runEditor: runTerminal, tui: tui.Run,
		launchctl: launchd.Real, agentWait: agentWait, answerWait: admin.AnswerWait,
		pidPoll: pidPoll, pidWait: pidWait, stdout: stdout, stderr: stderr}
	return c.run(args)
}

// run runs one command with c's environment and returns the exit code.
func (c *command) run(args []string) int {
	if len(args) == 0 {
		return c.startTUI()
	}
	cmd, rest := args[0], args[1:]
	switch {
	case cmd == "tui" && len(rest) == 0:
		return c.startTUI()
	case cmd == "help" || cmd == "-h" || cmd == "--help":
		fmt.Fprint(c.stdout, usage)
		return exitOK
	case cmd == "serve" && len(rest) == 0:
		return c.serve()
	case cmd == "status" && len(rest) <= 1:
		return c.status(rest)
	case cmd == "refresh" && len(rest) <= 2:
		return c.refresh(rest)
	case cmd == "reload" && len(rest) == 0:
		return c.reload()
	case cmd == "edit" && len(rest) <= 1:
		return c.edit(rest)
	case cmd == "init" && len(rest) == 1:
		return c.initProject(rest[0])
	case cmd == "install" && len(rest) == 0:
		return c.install()
	case cmd == "uninstall" && len(rest) == 0:
		return c.uninstall()
	case cmd == "log":
		if follow, ns, ok := logArgs(rest); ok {
			return c.log(follow, ns)
		}
	}
	fmt.Fprint(c.stderr, usage)
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

// command runs one CLI command with its environment and output. getwd, home,
// runEditor, tui and launchctl are fields so that tests never use the real
// current folder or home dir, start an editor, take over the terminal or
// touch the owner's launchd agent.
type command struct {
	getenv         func(string) string
	getwd          func() (string, error)
	home           func() (string, error)
	runEditor      func(dir string, argv []string) error
	tui            func(cl tui.Client, opt tui.Options) error
	launchctl      launchd.Launchctl
	agentWait      time.Duration // how long install waits for the agent's broker to answer
	answerWait     time.Duration // how long a request waits for an answer (admin.Waits.Answer)
	pidPoll        time.Duration // the gap between two reads of the agent's pid
	pidWait        time.Duration // how long install waits for the pid to hold still
	stdout, stderr io.Writer
}

// startTUI runs the TUI on the namespace of the project marker, if any. It
// asks the broker for its status first: with no broker it prints the
// no-broker line, with a broker that does not answer it prints that, and it
// exits 1; it never starts a broker. A bad marker does not stop it: the TUI
// starts with no namespace and shows the marker's error.
func (c *command) startTUI() int {
	cfg, err := c.configDir()
	if err != nil {
		return c.fail(err)
	}
	state, err := c.stateDir()
	if err != nil {
		return c.fail(err)
	}
	fetch, err := c.fetchTimeout() // a refresh waits for it
	if err != nil {
		return c.fail(err)
	}
	cl := tui.NewSocketClient(state, admin.Waits{Answer: c.answerWait, Fetch: fetch})
	if _, err := cl.Status(""); err != nil {
		return c.fail(err)
	}
	opt := tui.Options{ConfigDir: cfg, Editor: runOn, Getenv: c.getenv}
	opt.Namespace, _, err = c.namespace(nil)
	if err != nil && !errors.Is(err, errNoMarker) {
		// Escaped: the error names the marker's path, which the sandbox
		// can name. It never holds the marker's content.
		opt.Note = "marker: " + eventlog.Printable(err.Error())
	}
	if err := c.tui(cl, opt); err != nil {
		return c.fail(err)
	}
	return exitOK
}

// namespace returns args[0] when given, else the namespace of the project
// marker of the current folder and the marker's path. With neither it
// returns errNoMarker.
func (c *command) namespace(args []string) (ns, marker string, err error) {
	if len(args) > 0 {
		return args[0], "", nil
	}
	dir, err := c.getwd()
	if err != nil {
		return "", "", err
	}
	marker, ns, err = findMarker(dir)
	return ns, marker, err
}

// noNamespace reports a command that needs a namespace and found none.
func (c *command) noNamespace() int {
	fmt.Fprintf(c.stderr, "sbx-broker: %v\n", errNoMarker)
	fmt.Fprint(c.stderr, usage)
	return exitUsage
}

func (c *command) fail(err error) int {
	if errors.Is(err, admin.ErrNoBroker) {
		fmt.Fprintln(c.stderr, noBroker)
	} else {
		// Escaped: an error may name a path of the workspace, which the
		// sandbox can name.
		fmt.Fprintf(c.stderr, "sbx-broker: %s\n", eventlog.Printable(err.Error()))
	}
	return exitFail
}

// dir returns the env value of key, or the default under the home dir.
func (c *command) dir(key string, def ...string) (string, error) {
	if v := c.getenv(key); v != "" {
		return v, nil
	}
	home, err := c.home()
	if err != nil {
		return "", fmt.Errorf("%s is not set and %w", key, err)
	}
	return filepath.Join(append([]string{home}, def...)...), nil
}

func (c *command) stateDir() (string, error) {
	return c.dir("SBX_BROKER_STATE_DIR", ".local", "state", "sbx-broker")
}

// configDir is absolute: edit runs the editor in it, so a relative path
// would point the editor at the wrong file.
func (c *command) configDir() (string, error) {
	cfg, err := c.dir("SBX_BROKER_CONFIG_DIR", ".config", "sbx-broker", "namespaces")
	if err != nil {
		return "", err
	}
	return c.abs(cfg)
}

// abs returns path joined to the current folder when it is relative.
func (c *command) abs(path string) (string, error) {
	if filepath.IsAbs(path) {
		return path, nil
	}
	wd, err := c.getwd()
	if err != nil {
		return "", err
	}
	return filepath.Join(wd, path), nil
}

// fetchTimeout returns SBX_BROKER_FETCH_TIMEOUT, or 0 when it is not set.
func (c *command) fetchTimeout() (time.Duration, error) {
	v := c.getenv("SBX_BROKER_FETCH_TIMEOUT")
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("SBX_BROKER_FETCH_TIMEOUT %q is not a positive Go duration", v)
	}
	return d, nil
}

func (c *command) serve() int {
	cfg, err := c.configDir()
	if err != nil {
		return c.fail(err)
	}
	state, err := c.stateDir()
	if err != nil {
		return c.fail(err)
	}
	timeout, err := c.fetchTimeout()
	if err != nil {
		return c.fail(err)
	}
	opt := server.Options{ConfigDir: cfg, StateDir: state, FetchTimeout: timeout}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := server.Run(ctx, opt); err != nil {
		return c.fail(err)
	}
	return exitOK
}

// do sends one request to the broker. It waits c.answerWait for the answer,
// a refresh SBX_BROKER_FETCH_TIMEOUT more, a follow as long as it runs.
func (c *command) do(req admin.Request, each func(admin.Response) error) error {
	state, err := c.stateDir()
	if err != nil {
		return err
	}
	w := admin.Waits{Answer: c.answerWait}
	if req.Op == "refresh" {
		if w.Fetch, err = c.fetchTimeout(); err != nil {
			return err
		}
	}
	cl, err := admin.DialWaits(state, w)
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

// status shows one namespace (the argument or the marker's) or, outside a
// project, all of them.
func (c *command) status(args []string) int {
	cfg, err := c.configDir()
	if err != nil {
		return c.fail(err)
	}
	ns, marker, err := c.namespace(args)
	if err != nil && !errors.Is(err, errNoMarker) {
		return c.fail(err)
	}
	nss, err := c.namespaces(admin.Request{Op: "status", Namespace: ns})
	if err != nil {
		code := c.fail(err)
		if ns != "" && !errors.Is(err, admin.ErrNoBroker) && !errors.Is(err, admin.ErrNoAnswer) {
			if _, serr := os.Stat(filepath.Join(cfg, ns+".yaml")); errors.Is(serr, fs.ErrNotExist) {
				fmt.Fprintf(c.stderr, "create it with: sbx-broker init %s\n", ns)
			}
		}
		return code
	}
	if len(nss) == 0 {
		fmt.Fprintln(c.stdout, "no namespaces")
		return exitOK
	}
	if marker != "" {
		fmt.Fprintf(c.stdout, "marker: %s\n", eventlog.Printable(marker))
	}
	printStatus(c.stdout, nss, cfg)
	return exitOK
}

// refresh exits 1 when a refreshed secret is not ok afterwards.
func (c *command) refresh(args []string) int {
	cfg, err := c.configDir()
	if err != nil {
		return c.fail(err)
	}
	ns, _, err := c.namespace(args)
	if errors.Is(err, errNoMarker) {
		return c.noNamespace()
	}
	if err != nil {
		return c.fail(err)
	}
	req := admin.Request{Op: "refresh", Namespace: ns}
	if len(args) == 2 {
		req.Secret = args[1]
	}
	nss, err := c.namespaces(req)
	if err != nil {
		return c.fail(err)
	}
	printStatus(c.stdout, nss, cfg)
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

const (
	agentLabel = "io.github.abrose.sbx-broker"
	// agentWait is the 60 s default fetch timeout plus a margin.
	agentWait = 75 * time.Second
	agentPoll = 250 * time.Millisecond
	// install reads the agent's pid until two reads pidPoll apart agree,
	// for at most pidWait.
	pidPoll = time.Second
	pidWait = 5 * time.Second
)

// install runs serve as the launchd agent and waits for its broker to
// answer. It refuses while a broker that is not the agent answers (C8): two
// brokers would fight over the lock, and KeepAlive would restart the loser.
// It also refuses while a broker accepts on the socket but does not answer:
// that broker still holds the lock. The wait for the agent's broker ends
// after c.agentWait, however long one probe could wait.
func (c *command) install() int {
	a, err := c.agent()
	if err != nil {
		return c.fail(err)
	}
	state := filepath.Dir(a.LogPath)
	switch err := probe(state, c.answerWait); {
	case errors.Is(err, admin.ErrNoAnswer):
		return c.fail(fmt.Errorf("a broker holds %s but does not answer: stop it, then run sbx-broker install again",
			admin.SocketPath(state)))
	case err == nil:
		loaded, err := launchd.Loaded(c.launchctl, a.Label)
		if err != nil {
			return c.fail(err)
		}
		if !loaded {
			return c.fail(fmt.Errorf("a broker already runs on %s, not as the launchd agent: stop it, then run sbx-broker install again",
				admin.SocketPath(state)))
		}
	}
	if err := launchd.Install(a, c.launchctl); err != nil {
		return c.fail(err)
	}
	for deadline := time.Now().Add(c.agentWait); ; time.Sleep(agentPoll) {
		left := time.Until(deadline)
		if left <= 0 {
			fmt.Fprintf(c.stderr, "installed %s, but the broker does not answer; see %s\n", a.PlistPath, a.LogPath)
			return exitFail
		}
		if probe(state, min(c.answerWait, left)) == nil {
			break
		}
	}
	if err := c.agentRuns(a.Label); err != nil {
		return c.fail(err)
	}
	fmt.Fprintf(c.stdout, "installed %s\nthe broker answers on %s\n", a.PlistPath, admin.SocketPath(state))
	return exitOK
}

// errOtherBroker is the refusal when an answer does not come from the agent.
var errOtherBroker = errors.New("a broker answers, but it is not the launchd agent (the agent has no running process): stop the other broker, then run sbx-broker install again")

// agentRuns checks that the agent has a process of its own, as the answer may
// come from another broker that was not stopped. The agent's process must
// hold one pid for c.pidPoll: a process that fails on the other broker's lock
// is alive for a moment after bootstrap, then launchd throttles its restart.
// No running process, or no stable pid within c.pidWait, gives errOtherBroker.
func (c *command) agentRuns(label string) error {
	last, running, err := launchd.PID(c.launchctl, label)
	for deadline := time.Now().Add(c.pidWait); err == nil && running && time.Now().Before(deadline); {
		time.Sleep(c.pidPoll)
		var pid int
		if pid, running, err = launchd.PID(c.launchctl, label); err == nil && running && pid == last {
			return nil
		} else {
			last = pid
		}
	}
	if err != nil {
		return err
	}
	return errOtherBroker
}

func (c *command) uninstall() int {
	plist, err := c.plistPath()
	if err != nil {
		return c.fail(err)
	}
	removed, err := launchd.Uninstall(launchd.Agent{Label: agentLabel, PlistPath: plist}, c.launchctl)
	if err != nil {
		return c.fail(err)
	}
	if removed {
		fmt.Fprintf(c.stdout, "removed %s\n", plist)
	} else {
		fmt.Fprintln(c.stdout, "not installed")
	}
	return exitOK
}

func (c *command) plistPath() (string, error) {
	home, err := c.home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", agentLabel+".plist"), nil
}

// agent is the launchd agent of this shell: the running binary, the shell's
// PATH, and the SBX_BROKER_* variables that are set, the dirs made absolute.
// No other variable reaches the agent. Its log is serve.log in the state dir.
func (c *command) agent() (launchd.Agent, error) {
	plist, err := c.plistPath()
	if err != nil {
		return launchd.Agent{}, err
	}
	state, err := c.stateDir()
	if err == nil {
		state, err = c.abs(state)
	}
	if err != nil {
		return launchd.Agent{}, err
	}
	if _, err := c.fetchTimeout(); err != nil {
		return launchd.Agent{}, err
	}
	env := map[string]string{}
	for _, k := range []string{"PATH", "SBX_BROKER_CONFIG_DIR", "SBX_BROKER_STATE_DIR", "SBX_BROKER_FETCH_TIMEOUT"} {
		v := c.getenv(k)
		if v == "" {
			continue
		}
		if strings.HasSuffix(k, "_DIR") {
			if v, err = c.abs(v); err != nil {
				return launchd.Agent{}, err
			}
		}
		env[k] = v
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return launchd.Agent{}, fmt.Errorf("the path of this binary: %w", err)
	}
	return launchd.Agent{Label: agentLabel, PlistPath: plist, Program: exe, Env: env, LogPath: filepath.Join(state, "serve.log")}, nil
}

// probe asks the broker on the socket in state for its status and waits at
// most wait for the answer. nil: it answers; admin.ErrNoBroker: none runs;
// admin.ErrNoAnswer: one accepts but does not answer. wait <= 0 means
// admin.AnswerWait (5 s).
func probe(state string, wait time.Duration) error {
	cl, err := admin.DialWaits(state, admin.Waits{Answer: wait})
	if err != nil {
		return err
	}
	return cl.Do(admin.Request{Op: "status"}, func(admin.Response) error { return nil })
}

// log shows the events of one namespace (the argument or the marker's) or,
// outside a project, of all of them.
func (c *command) log(follow bool, ns string) int {
	if ns == "" {
		var err error
		if ns, _, err = c.namespace(nil); err != nil && !errors.Is(err, errNoMarker) {
			return c.fail(err)
		}
	}
	err := c.do(admin.Request{Op: "log", Namespace: ns, Follow: follow}, func(r admin.Response) error {
		if r.Event != nil {
			return printEvent(c.stdout, *r.Event)
		}
		return nil
	})
	if err != nil {
		return c.fail(err)
	}
	return exitOK
}

// printStatus writes one header line per namespace, the path of its file in
// cfgDir, and one aligned line per secret, sorted by name as the broker sends
// them.
func printStatus(w io.Writer, nss []admin.NamespaceStatus, cfgDir string) {
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
		fmt.Fprintf(w, "  file: %s\n", filepath.Join(cfgDir, ns.Name+".yaml"))
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
			fmt.Fprintf(w, "  error: %s\n", eventlog.Printable(ns.Error))
		}
	}
}

// failure is the exit code and the first stderr line of a failed fetch,
// escaped: the line comes from a tool.
func failure(s secrets.Status) string {
	msg := eventlog.Printable(s.Err)
	if s.ExitCode > 0 {
		if s.Err == "" || s.Err == fmt.Sprintf("exit status %d", s.ExitCode) {
			return fmt.Sprintf("exit %d", s.ExitCode)
		}
		return fmt.Sprintf("exit %d: %s", s.ExitCode, msg)
	}
	return msg
}

// printEvent writes one event as one line.
func printEvent(w io.Writer, e eventlog.Event) error {
	_, err := fmt.Fprintln(w, eventlog.Line(e))
	return err
}
