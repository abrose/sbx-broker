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
	"slices"
	"strings"
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

// Real runs /bin/launchctl.
func Real(args ...string) ([]byte, error) {
	return exec.Command("/bin/launchctl", args...).CombinedOutput()
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

// Loaded reports whether launchd knows the agent label in the user's domain.
func Loaded(lc Launchctl, label string) bool {
	_, err := lc("print", target(label))
	return err == nil
}

// Install writes the plist, creates the log dir, boots a loaded agent of the
// same label out, and bootstraps the agent, which starts it (RunAtLoad).
func Install(a Agent, lc Launchctl) error {
	if err := writeAtomic(a.PlistPath, a.Plist()); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.LogPath), logDirMode); err != nil {
		return err
	}
	if Loaded(lc, a.Label) {
		if err := run(lc, "bootout", target(a.Label)); err != nil {
			return err
		}
	}
	return run(lc, "bootstrap", domain(), a.PlistPath)
}

// Uninstall boots the agent out when it is loaded and removes its plist.
// removed is false when neither the agent nor the plist existed.
func Uninstall(a Agent, lc Launchctl) (removed bool, err error) {
	loaded := Loaded(lc, a.Label)
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
