// Package cases is the list of requests that prove the broker: each one goes
// through a route of the broker to the recorder mock. It is a library with
// the standard library only, so the integration test on the host and the e2e
// client inside a sandbox run the same cases.
package cases

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// The keys of the ph map of Run: a secret name of the namespace under test,
// or the two placeholders it must refuse.
const (
	APIKey       = "api-key"
	ClientSecret = "client-secret"
	OldSecret    = "old-secret" // the mock IdP rejects its value
	Pass         = "pass"
	Broken       = "broken"          // its fetch fails
	Undefined    = "undefined"       // a placeholder of a secret the namespace does not define
	OtherNS      = "other-namespace" // a placeholder of another namespace
)

// The keys of the routes map of Run.
const (
	RouteAPI = "api"
	RouteIdP = "idp"
)

// CaseHeader carries the case name in each request, so the recorder's
// records can be told apart.
const CaseHeader = "X-Case"

// TokenPath is the mock IdP's token endpoint.
const TokenPath = "/realms/test/protocol/openid-connect/token"

// Outcome is the answer the app got for one case.
type Outcome struct {
	Case    string
	Status  int // 0 when the request failed; Body then holds the error
	Headers http.Header
	Body    string
}

// request is one case: its route and how to build its request.
type request struct {
	name  string
	route string
	build func(base string, ph map[string]string) (*http.Request, error)
}

var all = []request{
	{"header", RouteAPI, func(base string, ph map[string]string) (*http.Request, error) {
		return get(base+"/echo", "Authorization", "Bearer "+ph[APIKey])
	}},
	{"base64-header", RouteAPI, func(base string, ph map[string]string) (*http.Request, error) {
		return get(base+"/echo", "X-API-Key", base64.StdEncoding.EncodeToString([]byte(ph[APIKey])))
	}},
	{"basic", RouteAPI, func(base string, ph map[string]string) (*http.Request, error) {
		return get(base+"/echo", "Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("user:"+ph[Pass])))
	}},
	{"query", RouteAPI, func(base string, ph map[string]string) (*http.Request, error) {
		return get(base+"/echo?key="+ph[Pass]+"&page=1", "", "")
	}},
	{"path", RouteAPI, func(base string, ph map[string]string) (*http.Request, error) {
		return get(base+"/echo/"+ph[Pass], "", "")
	}},
	{"form-token", RouteIdP, func(base string, ph map[string]string) (*http.Request, error) {
		return tokenRequest(base, ph[ClientSecret])
	}},
	{"form-token-rejected", RouteIdP, func(base string, ph map[string]string) (*http.Request, error) {
		return tokenRequest(base, ph[OldSecret])
	}},
	{"json", RouteAPI, func(base string, ph map[string]string) (*http.Request, error) {
		return post(base+"/echo", "application/json", fmt.Sprintf(`{"user":"app","password":%q}`, ph[Pass]))
	}},
	{"echo-header", RouteAPI, func(base string, ph map[string]string) (*http.Request, error) {
		return get(base+"/echo-header?name=X-Secret", "X-Secret", ph[APIKey])
	}},
	{"error", RouteAPI, func(base string, ph map[string]string) (*http.Request, error) {
		r, err := post(base+"/error", "text/plain", "secret="+ph[Pass])
		if err == nil {
			r.Header.Set("Authorization", "Bearer "+ph[APIKey])
			r.Header.Set("X-API-Key", ph[ClientSecret])
		}
		return r, err
	}},
	{"gzip", RouteAPI, func(base string, ph map[string]string) (*http.Request, error) {
		r, err := get(base+"/gzip", "Authorization", "Bearer "+ph[APIKey])
		if err == nil {
			// Set by hand, so the client never decodes the answer itself:
			// the outcome is what the broker sent.
			r.Header.Set("Accept-Encoding", "gzip")
		}
		return r, err
	}},
	{"sse", RouteAPI, func(base string, ph map[string]string) (*http.Request, error) {
		r, err := get(base+"/sse", "X-API-Key", ph[APIKey])
		if err == nil {
			r.Header.Set("Accept", "text/event-stream")
		}
		return r, err
	}},
	{"other-namespace", RouteAPI, func(base string, ph map[string]string) (*http.Request, error) {
		return get(base+"/echo", "Authorization", "Bearer "+ph[OtherNS])
	}},
	{"failed", RouteAPI, func(base string, ph map[string]string) (*http.Request, error) {
		return get(base+"/echo", "Authorization", "Bearer "+ph[Broken])
	}},
	{"undefined", RouteAPI, func(base string, ph map[string]string) (*http.Request, error) {
		return get(base+"/echo", "Authorization", "Bearer "+ph[Undefined])
	}},
}

// Run sends every case, one after the other, and returns their outcomes in
// order. routes maps a route name (RouteAPI, RouteIdP) to its base URL; ph
// maps a key (APIKey ... OtherNS) to its placeholder.
func Run(routes map[string]string, ph map[string]string, client *http.Client) []Outcome {
	out := make([]Outcome, 0, len(all))
	for _, c := range all {
		out = append(out, runOne(c, routes, ph, client))
	}
	return out
}

func runOne(c request, routes map[string]string, ph map[string]string, client *http.Client) Outcome {
	o := Outcome{Case: c.name}
	base, ok := routes[c.route]
	if !ok {
		o.Body = "error: no route " + c.route
		return o
	}
	req, err := c.build(strings.TrimSuffix(base, "/"), ph)
	if err != nil {
		o.Body = "error: " + err.Error()
		return o
	}
	req.Header.Set(CaseHeader, c.name)
	resp, err := client.Do(req)
	if err != nil {
		o.Body = "error: " + err.Error()
		return o
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	o.Status, o.Headers, o.Body = resp.StatusCode, resp.Header, string(body)
	if err != nil {
		o.Status, o.Body = 0, "error: read the body: "+err.Error()
	}
	return o
}

// get builds a GET request with one header, unless name is empty.
func get(u, name, value string) (*http.Request, error) {
	r, err := http.NewRequest(http.MethodGet, u, nil)
	if err == nil && name != "" {
		r.Header.Set(name, value)
	}
	return r, err
}

func post(u, contentType, body string) (*http.Request, error) {
	r, err := http.NewRequest(http.MethodPost, u, strings.NewReader(body))
	if err == nil {
		r.Header.Set("Content-Type", contentType)
	}
	return r, err
}

// tokenRequest is an OAuth client-credentials token request.
func tokenRequest(base, secret string) (*http.Request, error) {
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {"app"}, "client_secret": {secret}}
	return post(base+TokenPath, "application/x-www-form-urlencoded", form.Encode())
}
