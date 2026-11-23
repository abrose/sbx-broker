package main

import (
	"bytes"
	"encoding/json"
	"maps"
	"strings"
	"testing"

	"github.com/abrose/sbx-broker/test/pgcases"
)

const goodRoutes = "api=http://host.docker.internal:29691,idp=http://host.docker.internal:29692"

func TestPairs(t *testing.T) {
	got, err := pairs(goodRoutes)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"api": "http://host.docker.internal:29691", "idp": "http://host.docker.internal:29692"}
	if !maps.Equal(got, want) {
		t.Errorf("pairs: %v, want %v", got, want)
	}
}

// TestPostgresMode: -postgres with -placeholders is accepted and prints one
// outcome per Postgres case, each with a connection error, and exits 0.
func TestPostgresMode(t *testing.T) {
	var out, errOut bytes.Buffer
	// A closed loopback port: the connection is refused at once.
	if code := run([]string{"-postgres", "127.0.0.1:1", "-placeholders", "a=b"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d, want 0 (stderr %q)", code, errOut.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 9 {
		t.Fatalf("%d outcome lines, want 9:\n%s", len(lines), out.String())
	}
	for _, l := range lines {
		var o pgcases.Outcome
		if err := json.Unmarshal([]byte(l), &o); err != nil {
			t.Fatalf("line %q is no outcome: %v", l, err)
		}
		if o.Case == "" || o.Message == "" || o.Code != "" || o.Row != "" {
			t.Errorf("outcome %+v: want a case and a connection error without code or row", o)
		}
	}
}

// TestBadFlags: every bad flag value exits 2 with a message, and sends
// nothing (no outcome on stdout).
func TestBadFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
		msg  string
	}{
		{"duplicate key", []string{"-routes", "api=http://a:1,api=http://b:2", "-placeholders", "pass=sbxb_t1_pass"}, `key "api" is given twice`},
		{"missing =", []string{"-routes", goodRoutes, "-placeholders", "pass"}, `entry "pass" is not key=value`},
		{"empty value", []string{"-routes", goodRoutes, "-placeholders", "pass="}, `entry "pass=" is not key=value`},
		{"empty key", []string{"-routes", "=http://a:1", "-placeholders", "pass=sbxb_t1_pass"}, `entry "=http://a:1" is not key=value`},
		{"stray argument", []string{"-routes", goodRoutes, "-placeholders", "pass=sbxb_t1_pass", "extra"}, `unexpected argument "extra"`},
		{"no placeholders", []string{"-routes", goodRoutes}, "-placeholders is required"},
		{"unknown flag", []string{"-nope"}, "flag provided but not defined: -nope"},
		{"routes and postgres", []string{"-routes", goodRoutes, "-postgres", "h:1", "-placeholders", "a=b"}, "exactly one of -routes and -postgres"},
		{"neither routes nor postgres", []string{"-placeholders", "a=b"}, "exactly one of -routes and -postgres"},
		{"postgres without placeholders", []string{"-postgres", "h:1"}, "-placeholders is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := run(tt.args, &out, &errOut); code != 2 {
				t.Errorf("exit %d, want 2", code)
			}
			if out.Len() != 0 {
				t.Errorf("stdout %q, want nothing", out.String())
			}
			if !strings.Contains(errOut.String(), tt.msg) {
				t.Errorf("stderr %q, want it to hold %q", errOut.String(), tt.msg)
			}
		})
	}
}
