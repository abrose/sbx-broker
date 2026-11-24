// Package launchd runs the broker as a per-user launchd agent: it renders the
// agent's plist and drives launchctl in the gui/<uid> domain.
package launchd

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	plistMode    = 0o644 // the plist holds no secret: PATH and the broker's dirs
	plistDirMode = 0o755 // the mode of ~/Library/LaunchAgents when it is missing
	logDirMode   = 0o700 // the state dir, as the broker creates it
)

// Agent is one launchd agent that runs Program with the argument serve.
type Agent struct {
	Label     string
	PlistPath string
	Program   string            // an absolute path
	Env       map[string]string // EnvironmentVariables of the agent
	LogPath   string            // stdout and stderr of the agent
}

// Launchctl runs launchctl with args and returns its combined output.
type Launchctl func(args ...string) (out []byte, err error)

// ExitError is a launchctl that ran and exited with a non-zero code.
type ExitError struct {
	Code   int
	Output string // the combined output of launchctl
}

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// notFound is the exit code of launchctl print for a service launchd does
// not know.
const notFound = 113

// Real runs /bin/launchctl. A non-zero exit comes back as an *ExitError.
func Real(args ...string) ([]byte, error) {
	out, err := exec.Command("/bin/launchctl", args...).CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out, &ExitError{Code: ee.ExitCode(), Output: string(out)}
	}
	return out, err
}

// Plist returns the agent's plist. Its keys and the keys of Env are sorted,
// so the same agent gives the same bytes. Each value is XML-escaped.
func (a Agent) Plist() []byte {
	var b bytes.Buffer
	b.WriteString(xml.Header)
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	key(&b, 1, "EnvironmentVariables")
	b.WriteString("\t<dict>\n")
	names := make([]string, 0, len(a.Env))
	for k := range a.Env {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, k := range names {
		key(&b, 2, k)
		str(&b, 2, a.Env[k])
	}
	b.WriteString("\t</dict>\n")
	key(&b, 1, "KeepAlive")
	b.WriteString("\t<dict>\n")
	key(&b, 2, "SuccessfulExit")
	b.WriteString("\t\t<false/>\n\t</dict>\n")
	key(&b, 1, "Label")
	str(&b, 1, a.Label)
	key(&b, 1, "ProgramArguments")
	b.WriteString("\t<array>\n")
	str(&b, 2, a.Program)
	str(&b, 2, "serve")
	b.WriteString("\t</array>\n")
	key(&b, 1, "RunAtLoad")
	b.WriteString("\t<true/>\n")
	key(&b, 1, "StandardErrorPath")
	str(&b, 1, a.LogPath)
	key(&b, 1, "StandardOutPath")
	str(&b, 1, a.LogPath)
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes()
}

func key(b *bytes.Buffer, depth int, k string) { element(b, depth, "key", k) }
func str(b *bytes.Buffer, depth int, s string) { element(b, depth, "string", s) }

func element(b *bytes.Buffer, depth int, name, text string) {
	b.WriteString(strings.Repeat("\t", depth) + "<" + name + ">")
	_ = xml.EscapeText(b, []byte(text)) // a bytes.Buffer never fails
	b.WriteString("</" + name + ">\n")
}

// domain is the launchd domain of the user's GUI session.
func domain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

func target(label string) string { return domain() + "/" + label }

// printAgent runs launchctl print for the agent label and returns its output.
// A failure names the command and the output, as run does.
func printAgent(lc Launchctl, label string) ([]byte, error) {
	out, err := lc("print", target(label))
	return out, describe(out, err, "print", target(label))
}

// Loaded reports whether launchd knows the agent label in the user's domain.
// Only exit 113 (the service is not found) means not loaded; any other
// failure of launchctl is an error.
func Loaded(lc Launchctl, label string) (bool, error) {
	_, err := printAgent(lc, label)
	var ee *ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &ee) && ee.Code == notFound:
		return false, nil
	}
	return false, err
}

var pidLine = regexp.MustCompile(`(?m)^\s*pid = (\d+)\s*$`)

// PID returns the pid of the agent's process. ok is false when the agent is
// loaded and has no running process. A failing launchctl print, a not loaded
// agent included, is an error.
func PID(lc Launchctl, label string) (pid int, ok bool, err error) {
	out, err := printAgent(lc, label)
	if err != nil {
		return 0, false, err
	}
	m := pidLine.FindSubmatch(out)
	if m == nil {
		return 0, false, nil
	}
	pid, err = strconv.Atoi(string(m[1]))
	return pid, err == nil, err
}

// Install writes the plist, creates the log dir, boots a loaded agent of the
// same label out, and bootstraps the agent, which starts it (RunAtLoad).
func Install(a Agent, lc Launchctl) error { return install(a, lc, time.Sleep) }

func install(a Agent, lc Launchctl, sleep func(time.Duration)) error {
	if err := writeAtomic(a.PlistPath, a.Plist()); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.LogPath), logDirMode); err != nil {
		return err
	}
	loaded, err := Loaded(lc, a.Label)
	if err != nil {
		return err
	}
	if loaded {
		if err := run(lc, "bootout", target(a.Label)); err != nil {
			return err
		}
		if err := waitUnloaded(lc, a.Label, sleep); err != nil {
			return err
		}
	}
	if err := run(lc, "bootstrap", domain(), a.PlistPath); err != nil {
		return fmt.Errorf("%w; the plist stays at %s, sbx-broker uninstall removes it", err, a.PlistPath)
	}
	return nil
}

const (
	unloadPoll  = 100 * time.Millisecond
	unloadPolls = 100 // 10 s
)

// waitUnloaded waits until launchd no longer knows label. A bootstrap right
// after a bootout fails with an I/O error while launchd still tears the old
// service down, and the old broker could still answer the new one's poll.
func waitUnloaded(lc Launchctl, label string, sleep func(time.Duration)) error {
	for range unloadPolls {
		loaded, err := Loaded(lc, label)
		if err != nil || !loaded {
			return err
		}
		sleep(unloadPoll)
	}
	return fmt.Errorf("the agent %s is still loaded %v after bootout; run install again", label, unloadPoll*unloadPolls)
}

// Uninstall boots the agent out when it is loaded and removes its plist.
// removed is false when neither the agent nor the plist existed.
func Uninstall(a Agent, lc Launchctl) (removed bool, err error) {
	loaded, err := Loaded(lc, a.Label)
	if err != nil {
		return false, err
	}
	if loaded {
		if err := run(lc, "bootout", target(a.Label)); err != nil {
			return false, err
		}
	}
	err = os.Remove(a.PlistPath)
	if errors.Is(err, fs.ErrNotExist) {
		return loaded, nil
	}
	return err == nil, err
}

// run runs launchctl and names the command and its output in an error.
func run(lc Launchctl, args ...string) error {
	out, err := lc(args...)
	return describe(out, err, args...)
}

// describe is nil for a nil err; else it names the command and its output.
func describe(out []byte, err error, args ...string) error {
	if err == nil {
		return nil
	}
	if msg := strings.TrimSpace(string(out)); msg != "" {
		return fmt.Errorf("launchctl %s: %s (%w)", strings.Join(args, " "), msg, err)
	}
	return fmt.Errorf("launchctl %s: %w", strings.Join(args, " "), err)
}

// writeAtomic writes data to a temp file in the dir of path and renames it
// over path, so launchd never reads a half-written plist.
func writeAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, plistDirMode); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}
	}()
	if err := f.Chmod(plistMode); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
