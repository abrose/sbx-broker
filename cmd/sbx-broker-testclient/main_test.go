package main

import (
	"bytes"
	"maps"
	"strings"
	"testing"
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
		{"no placeholders", []string{"-routes", goodRoutes}, "both -routes and -placeholders are required"},
		{"unknown flag", []string{"-nope"}, "flag provided but not defined: -nope"},
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
