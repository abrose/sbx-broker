// Command recorder is the mock upstream of the integration proof. Port 8080
// serves the cases and records every request; port 8081 serves the records.
// It uses the standard library only, so its image builds offline.
package main

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	maxBody   = 1 << 20
	tokenPath = "/realms/test/protocol/openid-connect/token"
	sseGap    = 200 * time.Millisecond
)

// Record is one request as the recorder got it. Path is the decoded path,
// RawPath the escaped one.
type Record struct {
	Method   string              `json:"method"`
	Path     string              `json:"path"`
	RawPath  string              `json:"rawPath"`
	RawQuery string              `json:"rawQuery"`
	Headers  map[string][]string `json:"headers"`
	Body     string              `json:"body"`
}

type recorder struct {
	mu      sync.Mutex
	records []Record
	secret  string // the client secret the token endpoint accepts
}

func main() {
	rec := &recorder{secret: os.Getenv("MOCK_CLIENT_SECRET")}
	errs := make(chan error, 2)
	go func() { errs <- http.ListenAndServe(":8080", http.HandlerFunc(rec.cases)) }()
	go func() { errs <- http.ListenAndServe(":8081", http.HandlerFunc(rec.serveRecords)) }()
	log.Fatal(<-errs)
}

// serveRecords answers GET /records with every record and DELETE /records
// by clearing them.
func (rec *recorder) serveRecords(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/records" {
		http.NotFound(w, r)
		return
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		all := rec.records
		if all == nil {
			all = []Record{}
		}
		writeJSON(w, http.StatusOK, all)
	case http.MethodDelete:
		rec.records = nil
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// cases records the request, then answers it as its path says.
func (rec *recorder) cases(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	got := Record{Method: r.Method, Path: r.URL.Path, RawPath: r.URL.EscapedPath(), RawQuery: r.URL.RawQuery, Headers: r.Header, Body: string(body)}
	rec.mu.Lock()
	rec.records = append(rec.records, got)
	rec.mu.Unlock()

	switch p := r.URL.Path; {
	case p == "/echo" || strings.HasPrefix(p, "/echo/"):
		writeJSON(w, http.StatusOK, got)
	case p == "/echo-header":
		w.Header().Set("X-Echo", r.Header.Get(r.URL.Query().Get("name")))
		_, _ = io.WriteString(w, "ok\n")
	case p == "/error":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprintf(w, "unauthorized\nauthorization: %s\nx-api-key: %s\nbody: %s\n",
			r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), body)
	case p == "/gzip":
		gzipEcho(w, r, got)
	case p == "/sse":
		sse(w, r.Header.Get("X-Api-Key"))
	case p == tokenPath:
		rec.token(w, r, string(body))
	default:
		http.NotFound(w, r)
	}
}

// gzipEcho answers the echo gzip-compressed when the request allows gzip:
// as RFC 9110 says, a request without Accept-Encoding allows any coding.
func gzipEcho(w http.ResponseWriter, r *http.Request, got Record) {
	data, _ := json.Marshal(got) // a Record always encodes
	if !allowsGzip(r.Header.Values("Accept-Encoding")) {
		// Never hit through the broker: it strips Accept-Encoding, and no
		// Accept-Encoding allows any coding, so the proof gets gzip.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Set("X-Mock-Encoding", "gzip")
	zw := gzip.NewWriter(w)
	_, _ = zw.Write(data)
	_ = zw.Close()
}

func allowsGzip(accept []string) bool {
	if len(accept) == 0 {
		return true
	}
	for _, list := range accept {
		for _, item := range strings.Split(list, ",") {
			coding, params, _ := strings.Cut(strings.TrimSpace(item), ";")
			coding = strings.ToLower(strings.TrimSpace(coding))
			if (coding == "gzip" || coding == "*") && !zeroQ(params) {
				return true
			}
		}
	}
	return false
}

func zeroQ(params string) bool {
	q, ok := strings.CutPrefix(strings.ReplaceAll(params, " ", ""), "q=")
	return ok && strings.Trim(q, "0.") == ""
}

// sse streams "data: <key>\n\n" in two writes, split in the middle of key.
func sse(w http.ResponseWriter, key string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flush := http.NewResponseController(w).Flush
	half := len(key) / 2
	_, _ = io.WriteString(w, "data: "+key[:half])
	_ = flush()
	time.Sleep(sseGap)
	_, _ = io.WriteString(w, key[half:]+"\n\n")
	_ = flush()
}

// token is the mock IdP's token endpoint: it accepts the configured client
// secret, and quotes a wrong one in its error, as real IdPs do.
func (rec *recorder) token(w http.ResponseWriter, r *http.Request, body string) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	form, err := url.ParseQuery(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	got := form.Get("client_secret")
	if rec.secret == "" || got != rec.secret {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error": "invalid_client", "error_description": "bad secret " + got,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"access_token": "tok-1", "expires_in": 300})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
