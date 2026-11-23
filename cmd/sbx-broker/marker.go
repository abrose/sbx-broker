package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/abrose/sbx-broker/internal/placeholder"
)

// markerName is the project marker: a file whose first line names the
// project's namespace. The sandbox can write it, so it holds only a name,
// which the CLI maps to the config folder. serve never reads it.
const markerName = ".sbx-broker"

// maxMarker is the most the CLI reads of a marker.
const maxMarker = 4 << 10

var errNoMarker = errors.New("no namespace given, and no .sbx-broker marker in this folder or above")

// findMarker looks for the marker in dir and then in each parent folder, and
// reads the first one it finds. An entry of that name of any type stops the
// lookup: a bad marker is an error, never skipped.
func findMarker(dir string) (path, ns string, err error) {
	dir = filepath.Clean(dir)
	for {
		path = filepath.Join(dir, markerName)
		_, err := os.Lstat(path)
		switch {
		case err == nil:
			ns, err := readMarker(path)
			return path, ns, err
		case !errors.Is(err, fs.ErrNotExist):
			return path, "", fmt.Errorf("%s: cannot read the marker", path)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", errNoMarker
		}
		dir = parent
	}
}

// readMarker returns the namespace name of the marker at path. No error holds
// the marker's content: a marker may be made to point at a file of the Mac.
func readMarker(path string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("%s: cannot read the marker", path)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%s is a symlink", path)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	// The sandbox can swap the marker after the Lstat: open with no symlink
	// follow and no block on a FIFO, then check the open file itself.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("%s: cannot read the marker", path)
	}
	defer func() { _ = f.Close() }()
	if ofi, err := f.Stat(); err != nil || !ofi.Mode().IsRegular() || !os.SameFile(fi, ofi) {
		return "", fmt.Errorf("%s changed while it was read", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxMarker))
	if err != nil {
		return "", fmt.Errorf("%s: cannot read the marker", path)
	}
	line, _, _ := bytes.Cut(data, []byte("\n"))
	ns := strings.TrimSpace(string(line))
	if !placeholder.ValidNamespace(ns) {
		return "", fmt.Errorf("%s: not a valid namespace name (want 1-16 characters of a-z, 0-9)", path)
	}
	return ns, nil
}
