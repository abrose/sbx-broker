package harness

import (
	"context"
	"fmt"
	"io"
	"net/netip"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/abrose/sbx-broker/internal/eventlog"
	"github.com/abrose/sbx-broker/test/pgcases"
)

const (
	postgresImage = "postgres:17-bookworm"
	postgresPort  = "5432/tcp"
	// logWait is how long the container log may lag behind a login.
	logWait = 5 * time.Second
)

// StartPostgres runs a real Postgres in a container, with TLS from the
// snakeoil certificate of the Debian image (the broker logs in with
// sslmode=require) and the default scram-sha-256 password check of the image,
// and terminates it when the test ends. The port is published on 127.0.0.1
// only. It returns the host:port of the server and a function that reads the
// server's log.
func StartPostgres(t *testing.T) (upstream string, logs func() string) {
	t.Helper()
	ctx := context.Background()
	ctr, err := testcontainers.Run(ctx, postgresImage,
		testcontainers.WithEnv(map[string]string{
			"POSTGRES_USER":     "app",
			"POSTGRES_DB":       "appdb",
			"POSTGRES_PASSWORD": ValueDBPass,
		}),
		testcontainers.WithCmd("postgres",
			"-c", "ssl=on",
			"-c", "ssl_cert_file=/etc/ssl/certs/ssl-cert-snakeoil.pem",
			"-c", "ssl_key_file=/etc/ssl/private/ssl-cert-snakeoil.key",
			"-c", "log_connections=on"),
		testcontainers.WithExposedPorts(postgresPort),
		testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
			hc.PortBindings = network.PortMap{
				network.MustParsePort(postgresPort): []network.PortBinding{{HostIP: netip.MustParseAddr("127.0.0.1")}},
			}
		}),
		// The image starts a temporary server for its init, then the real
		// one: the second "ready" line is the real one.
		testcontainers.WithWaitStrategy(wait.ForAll(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
			wait.ForListeningPort(postgresPort),
		).WithDeadline(60*time.Second)),
	)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	p, err := ctr.MappedPort(ctx, postgresPort)
	if err != nil {
		t.Fatalf("mapped port %s: %v", postgresPort, err)
	}
	logs = func() string {
		r, err := ctr.Logs(context.Background())
		if err != nil {
			t.Errorf("read the postgres log: %v", err)
			return ""
		}
		defer r.Close()
		b, err := io.ReadAll(r)
		if err != nil {
			t.Errorf("read the postgres log: %v", err)
		}
		return string(b)
	}
	return "127.0.0.1:" + p.Port(), logs
}

// PGExpect is what one case of pgcases must give.
type PGExpect struct {
	Code       string // the SQLSTATE; "" when the login works
	Message    string // the exact message of the error
	Violation  string // the message of the violation event the case also logs, if any
	Row        string // the login query's row, "" for no row
	Kind       eventlog.Kind
	Route      string // the alias, or the app's database for an unknown one
	User       string
	Secrets    []string // the names in the event
	Authorized bool     // the upstream log shows the login
	Cancelled  bool     // the case cancels a 30 s query
}

// PGExpects maps each case of pgcases to what it must give.
var PGExpects = map[string]PGExpect{
	"pg-login":  {Row: "app appdb true", Kind: eventlog.KindSwap, Route: "app", User: "app", Secrets: []string{"db-pass"}, Authorized: true},
	"pg-cancel": {Code: "57014", Message: "canceling statement due to user request", Kind: eventlog.KindSwap, Route: "app", User: "app", Secrets: []string{"db-pass"}, Authorized: true, Cancelled: true},
	"pg-other-namespace": {Code: "28000", Message: "placeholder of another namespace", Violation: "placeholder of namespace t2",
		Kind: eventlog.KindRefuse, Route: "app", User: "app"},
	"pg-failed": {Code: "57P03", Message: "secret not loaded: broken (failed)", Kind: eventlog.KindRefuse, Route: "down", User: "app", Secrets: []string{"broken"}},
	"pg-wrong-placeholder": {Code: "28P01", Message: `password is not the placeholder of database "app"`,
		Kind: eventlog.KindRefuse, Route: "app", User: "app"},
	"pg-unknown-db": {Code: "3D000", Message: `database "nope" is not configured in namespace t1`, Kind: eventlog.KindRefuse, Route: "nope", User: "app"},
	"pg-wrong-user": {Code: "28000", Message: `user "intruder" may not use database "app"`, Kind: eventlog.KindRefuse, Route: "app", User: "intruder"},
	"pg-upstream-refused": {Code: "28P01", Message: `upstream: password authentication failed for user "app"`,
		Kind: eventlog.KindUpstream, Route: "stale", User: "app", Secrets: []string{"old-secret"}},
	// The sentinel runs after every refused case and must reach the upstream,
	// so its log line proves the log is complete before the negatives are read.
	"pg-sentinel": {Row: "app appdb true", Kind: eventlog.KindSwap, Route: "app", User: "app", Secrets: []string{"db-pass"}, Authorized: true},
}

// maxCancelMS is how long the cancelled query may take: the query sleeps 30 s.
const maxCancelMS = 10_000

// AssertPostgres checks the outcomes of pgcases.Run against PGExpects: the
// codes, the row of the login, no value in any message or row, and, in the
// upstream's log, a login line with the case's application_name for the cases
// that reach the upstream and none for the others.
func AssertPostgres(t *testing.T, outcomes []pgcases.Outcome, upstreamLog string) {
	t.Helper()
	byCase := map[string]pgcases.Outcome{}
	for _, o := range outcomes {
		byCase[o.Case] = o
	}
	if len(byCase) != len(PGExpects) || len(outcomes) != len(PGExpects) {
		t.Fatalf("got %d outcomes, want %d", len(outcomes), len(PGExpects))
	}
	names := make([]string, 0, len(PGExpects))
	for n := range PGExpects {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		want := PGExpects[name]
		o, ok := byCase[name]
		if !ok {
			t.Errorf("case %s: no outcome", name)
			continue
		}
		if o.Code != want.Code {
			t.Errorf("case %s: code %q, want %q (message %q)", name, o.Code, want.Code, o.Message)
		}
		if o.Message != want.Message {
			t.Errorf("case %s: message %q, want %q", name, o.Message, want.Message)
		}
		if o.Row != want.Row {
			t.Errorf("case %s: row %q, want %q", name, o.Row, want.Row)
		}
		if want.Cancelled && (o.MS <= 0 || o.MS >= maxCancelMS) {
			t.Errorf("case %s: the cancelled query took %d ms, want under %d", name, o.MS, maxCancelMS)
		}
		NoValue(t, "case "+name+" message", o.Message)
		NoValue(t, "case "+name+" row", o.Row)
		line := authorizedLine(upstreamLog, name)
		switch {
		case want.Authorized && line == "":
			t.Errorf("case %s: the upstream log has no login with application_name=%s", name, name)
		case want.Authorized && name == "pg-login" && !strings.Contains(line, "SSL enabled"):
			t.Errorf("case %s: the upstream login is not TLS: %q", name, line)
		case !want.Authorized && line != "":
			t.Errorf("case %s: the upstream log has a login the case must not reach: %q", name, line)
		}
	}
}

// authorizedLine returns the "connection authorized" line of the upstream log
// for the application name, or "".
func authorizedLine(upstreamLog, appName string) string {
	re := regexp.MustCompile(`connection authorized:.*application_name=` + regexp.QuoteMeta(appName) + `( |$)`)
	for _, l := range strings.Split(upstreamLog, "\n") {
		if re.MatchString(l) {
			return l
		}
	}
	return ""
}

// WaitUpstreamLog reads the upstream's log until it holds a login line for
// every case that must reach the upstream, at most logWait, and returns it.
// The container log may lag a moment behind a login.
func WaitUpstreamLog(logs func() string) string {
	deadline := time.Now().Add(logWait)
	for {
		l := logs()
		complete := true
		for name, want := range PGExpects {
			if want.Authorized && authorizedLine(l, name) == "" {
				complete = false
			}
		}
		if complete || time.Now().After(deadline) {
			return l
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// IsPGEvent reports whether e is the swap, refuse, upstream or violation
// event of a Postgres login (it names the app's user).
func IsPGEvent(e eventlog.Event) bool {
	switch e.Kind {
	case eventlog.KindSwap, eventlog.KindRefuse, eventlog.KindUpstream, eventlog.KindViolation:
		return e.User != ""
	}
	return false
}

// PGEventKey is the comparable form of a Postgres event: kind, namespace,
// user, route, secrets and the whole message.
func PGEventKey(e eventlog.Event) string {
	return pgKey(e.Kind, e.Namespace, e.Route, e.User, e.Secrets, e.Message)
}

// PGWantEvents returns the keys of the events the cases must give, sorted: the
// swap, refuse or upstream event of each case, and the violation event of a
// case that logs one.
func PGWantEvents() []string {
	var out []string
	for _, w := range PGExpects {
		msg := "login"
		if w.Kind != eventlog.KindSwap {
			msg = w.Code + " " + w.Message
		}
		out = append(out, pgKey(w.Kind, "t1", w.Route, w.User, w.Secrets, msg))
		if w.Violation != "" {
			out = append(out, pgKey(eventlog.KindViolation, "t1", w.Route, w.User, nil, w.Violation))
		}
	}
	sort.Strings(out)
	return out
}

func pgKey(kind eventlog.Kind, ns, route, user string, secrets []string, msg string) string {
	s := slices.Clone(secrets)
	sort.Strings(s)
	return fmt.Sprintf("%s %s %s@%s %v %s", kind, ns, user, route, s, msg)
}
