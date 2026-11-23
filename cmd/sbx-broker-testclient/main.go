// Command sbx-broker-testclient plays the app of the end-to-end proof: it
// runs inside a sandbox, sends the cases of test/cases through the broker's
// routes, and prints one JSON outcome per line. It uses the proxy of its
// environment, as an app in a sandbox does.
//
//	sbx-broker-testclient -routes <name=url,...> -placeholders <secret=placeholder,...>
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
	if err := fs.Parse(args); err != nil {
		return 2
	}
	r, err := pairs(*routes)
	if err == nil && fs.NArg() > 0 {
		err = fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	var ph map[string]string
	if err == nil {
		ph, err = pairs(*placeholders)
	}
	if err != nil {
		fmt.Fprintf(stderr, "sbx-broker-testclient: %v\n", err)
		return 2
	}
	client := &http.Client{Timeout: requestTimeout, Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment, DisableKeepAlives: true,
	}}
	enc := json.NewEncoder(stdout)
	for _, o := range cases.Run(r, ph, client) {
		if err := enc.Encode(o); err != nil {
			fmt.Fprintf(stderr, "sbx-broker-testclient: %v\n", err)
			return 1
		}
	}
	return 0
}

// pairs parses "k=v,k=v" into a map. It refuses an empty list, an entry
// with no "=" or an empty key, and a key given twice.
func pairs(s string) (map[string]string, error) {
	if s == "" {
		return nil, fmt.Errorf("both -routes and -placeholders are required")
	}
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
