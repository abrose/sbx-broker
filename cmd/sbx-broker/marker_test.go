package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func TestFindMarkerInFolder(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, markerName), "acme\n", 0o644)
	path, ns, err := findMarker(dir)
	if err != nil || path != filepath.Join(dir, markerName) || ns != "acme" {
		t.Fatalf("path %q, ns %q, err %v", path, ns, err)
	}
}

func TestFindMarkerInParent(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, markerName), "acme", 0o644)
	dir := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path, ns, err := findMarker(dir)
	if err != nil || path != filepath.Join(root, markerName) || ns != "acme" {
		t.Fatalf("path %q, ns %q, err %v", path, ns, err)
	}
}

func TestFindMarkerNearestWins(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, markerName), "outer", 0o644)
	writeFile(t, filepath.Join(root, "a", markerName), "inner", 0o644)
	dir := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, ns, err := findMarker(dir); err != nil || ns != "inner" {
		t.Fatalf("ns %q, err %v", ns, err)
	}
}

func TestFindMarkerNone(t *testing.T) {
	if _, _, err := findMarker(t.TempDir()); !errors.Is(err, errNoMarker) {
		t.Fatalf("err %v", err)
	}
}

func TestMarkerTrimsCRLF(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, markerName), "acme\r\nmore lines\n", 0o644)
	if _, ns, err := findMarker(dir); err != nil || ns != "acme" {
		t.Fatalf("ns %q, err %v", ns, err)
	}
}

func TestMarkerRefusals(t *testing.T) {
	const secret = "s3cr3t"
	cases := []struct {
		name string
		make func(t *testing.T, root, marker string)
	}{
		{"symlink", func(t *testing.T, root, marker string) {
			target := filepath.Join(t.TempDir(), "key")
			writeFile(t, target, secret, 0o600)
			if err := os.Symlink(target, marker); err != nil {
				t.Fatal(err)
			}
		}},
		{"folder", func(t *testing.T, root, marker string) {
			writeFile(t, filepath.Join(root, markerName), "acme", 0o644)
			if err := os.Mkdir(marker, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"path", func(t *testing.T, root, marker string) { writeFile(t, marker, "../"+secret, 0o644) }},
		{"empty", func(t *testing.T, root, marker string) { writeFile(t, marker, "", 0o644) }},
		{"too long", func(t *testing.T, root, marker string) { writeFile(t, marker, secret+"abcdefghijk", 0o644) }},
		{"unreadable", func(t *testing.T, root, marker string) {
			if os.Getuid() == 0 {
				t.Skip("root reads every file")
			}
			writeFile(t, marker, secret, 0o000)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "project")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(dir, markerName)
			tc.make(t, root, marker)
			_, _, err := findMarker(dir)
			if err == nil || errors.Is(err, errNoMarker) {
				t.Fatalf("err %v", err)
			}
			if !strings.Contains(err.Error(), marker) {
				t.Errorf("the error does not name the marker: %v", err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("the error holds the content: %v", err)
			}
		})
	}
}
