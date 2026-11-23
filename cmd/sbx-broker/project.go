package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/abrose/sbx-broker/internal/eventlog"
	"github.com/abrose/sbx-broker/internal/placeholder"
	"github.com/abrose/sbx-broker/internal/tui"
)

// namespaceTemplate is the file that init creates: comments only, so the
// broker loads it as an empty namespace until the owner fills it.
const namespaceTemplate = `# An sbx-broker namespace: the secrets and services of one project.
# The broker reads this file again within about 2 s of a change.
# Reference: https://github.com/abrose/sbx-broker/blob/main/docs/configuration.md
#
# secrets:
#   api-key:                  # the placeholder is sbxb_<namespace>_api_key (each - as _)
#     command: ["op", "read", "op://<vault>/<item>/credential"]
#
# http:
#   - name: api
#     listen: 29610           # the broker's port for this route, 1024 to 65535
#     upstream: https://<host>
`

// edit opens the namespace file of args[0] or of the project marker.
func (c *command) edit(args []string) int {
	ns, _, err := c.namespace(args)
	if errors.Is(err, errNoMarker) {
		return c.noNamespace()
	}
	if err != nil {
		return c.fail(err)
	}
	if !placeholder.ValidNamespace(ns) {
		return c.badName()
	}
	cfg, err := c.configDir()
	if err != nil {
		return c.fail(err)
	}
	path := filepath.Join(cfg, ns+".yaml")
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(c.stderr, "sbx-broker: no namespace file %s (create it with: sbx-broker init %s)\n", path, ns)
		return exitFail
	} else if err != nil {
		return c.fail(err)
	}
	// The editor runs in the config folder, never in the project folder: the
	// agent can write that one, and some editors load config from it.
	if err := c.runEditor(cfg, tui.EditorArgv(c.getenv, path)); err != nil {
		return c.fail(err)
	}
	return exitOK
}

// initProject marks the current folder as a project of ns and creates the
// namespace file from the template. It never overwrites a namespace file and
// never writes through a symlink.
func (c *command) initProject(ns string) int {
	if !placeholder.ValidNamespace(ns) {
		return c.badName()
	}
	dir, err := c.getwd()
	if err != nil {
		return c.fail(err)
	}
	cfg, err := c.configDir()
	if err != nil {
		return c.fail(err)
	}
	marker := filepath.Join(dir, markerName)
	shown := eventlog.Printable(marker) // the sandbox can name the folders
	switch _, err := os.Lstat(marker); {
	case err == nil:
		have, err := readMarker(marker)
		if err != nil {
			return c.fail(err)
		}
		if have != ns {
			fmt.Fprintf(c.stderr, "sbx-broker: %s names another namespace\n", shown)
			return exitFail
		}
		fmt.Fprintf(c.stdout, "kept %s\n", shown)
	case errors.Is(err, fs.ErrNotExist):
		if err := writeNew(marker, ns+"\n", 0o644); err != nil {
			return c.fail(fmt.Errorf("%s: cannot write the marker", marker))
		}
		fmt.Fprintf(c.stdout, "wrote %s\n", shown)
	default:
		return c.fail(fmt.Errorf("%s: cannot read the marker", marker))
	}
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		return c.fail(err)
	}
	path := filepath.Join(cfg, ns+".yaml")
	switch err := writeNew(path, namespaceTemplate, 0o600); {
	case err == nil:
		fmt.Fprintf(c.stdout, "created %s\n", path)
	case errors.Is(err, fs.ErrExist):
		fmt.Fprintf(c.stdout, "kept %s\n", path)
	default:
		return c.fail(err)
	}
	fmt.Fprintln(c.stdout, "next: sbx-broker edit")
	return exitOK
}

func (c *command) badName() int {
	fmt.Fprintln(c.stderr, "sbx-broker: not a valid namespace name (want 1-16 characters of a-z, 0-9)")
	return exitUsage
}

// writeNew creates path with content. It fails with fs.ErrExist when path
// exists, also as a symlink, so it never overwrites or writes through a link.
func writeNew(path, content string, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, perm)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// runTerminal runs argv in dir, on the terminal of the CLI.
func runTerminal(dir string, argv []string) error {
	return runOn(dir, argv, tui.Terminal{In: os.Stdin, Out: os.Stdout, Err: os.Stderr})
}

// runOn runs argv in dir on term: the TUI's editor key hands over the
// terminal Bubble Tea gives it.
func runOn(dir string, argv []string, term tui.Terminal) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = term.In, term.Out, term.Err
	return cmd.Run()
}
