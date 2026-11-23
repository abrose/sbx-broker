// Command sbx-broker-testclient plays the app of the end-to-end proof: it
// runs inside a sandbox, sends the cases of test/cases through the broker's
// routes, and prints one JSON outcome per line. It uses the proxy of its
// environment, as an app in a sandbox does. With -postgres it runs the
// Postgres cases of test/pgcases against the broker's listener instead.
//
//	sbx-broker-testclient -routes <name=url,...> -placeholders <secret=placeholder,...>
//	sbx-broker-testclient -postgres <host:port> -placeholders <secret=placeholder,...>
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/abrose/sbx-broker/test/cases"
	"github.com/abrose/sbx-broker/test/pgcases"
)

const requestTimeout = 30 * time.Second

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sbx-broker-testclient", flag.ContinueOnError)
	fs.SetOutput(stderr)
	routes := fs.String("routes", "", "the routes as name=url,...")
	placeholders := fs.String("placeholders", "", "the placeholders as secret=placeholder,...")
	postgres := fs.String("postgres", "", "the host:port of the broker's Postgres listener, instead of -routes")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	r, ph, err := parseArgs(fs, *routes, *postgres, *placeholders)
	if err != nil {
		fmt.Fprintf(stderr, "sbx-broker-testclient: %v\n", err)
		return 2
	}
	enc := json.NewEncoder(stdout)
	var outcomes []any
	if *postgres != "" {
		for _, o := range pgcases.Run(*postgres, ph) {
			outcomes = append(outcomes, o)
		}
	} else {
		client := &http.Client{Timeout: requestTimeout, Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment, DisableKeepAlives: true,
		}}
		for _, o := range cases.Run(r, ph, client) {
			outcomes = append(outcomes, o)
		}
	}
	for _, o := range outcomes {
		if err := enc.Encode(o); err != nil {
			fmt.Fprintf(stderr, "sbx-broker-testclient: %v\n", err)
			return 1
		}
	}
	return 0
}

// parseArgs checks the flags: exactly one of -routes and -postgres, and
// -placeholders always. It returns the routes (nil in Postgres mode) and the
// placeholders.
func parseArgs(fs *flag.FlagSet, routes, postgres, placeholders string) (r, ph map[string]string, err error) {
	if fs.NArg() > 0 {
		return nil, nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if (routes == "") == (postgres == "") {
		return nil, nil, fmt.Errorf("exactly one of -routes and -postgres is required")
	}
	if placeholders == "" {
		return nil, nil, fmt.Errorf("-placeholders is required")
	}
	if routes != "" {
		if r, err = pairs(routes); err != nil {
			return nil, nil, err
		}
	}
	ph, err = pairs(placeholders)
	return r, ph, err
}

// pairs parses "k=v,k=v" into a map. It refuses an empty list, an entry
// with no "=" or an empty key, and a key given twice.
func pairs(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, entry := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(entry, "=")
		if !ok || k == "" || v == "" {
			return nil, fmt.Errorf("entry %q is not key=value", entry)
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("key %q is given twice", k)
		}
		out[k] = v
	}
	return out, nil
}
