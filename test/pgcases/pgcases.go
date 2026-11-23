// Package pgcases is the list of Postgres logins that prove the broker's
// Postgres proxy: each one connects to the proxy's listener as an app would,
// with a placeholder as the password. It is for tests and the test client, not
// for the broker.
package pgcases

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/abrose/sbx-broker/test/cases"
)

// DBPass is the key of the placeholder of the database password of the
// namespace under test, in the ph map of Run. The other keys are those of
// package cases: OtherNS, Broken, APIKey and OldSecret.
const DBPass = "db-pass"

// Outcome is what the app got for one case.
type Outcome struct {
	Case    string
	Code    string // the SQLSTATE of the error, "" when the case worked
	Message string // the message of the error, or the text of another failure
	Row     string // the row of the login query
	MS      int64  // the time of the cancelled query
}

const (
	connectTimeout = 10 // seconds
	queryTimeout   = 40 * time.Second
	cancelAfter    = time.Second
)

// loginQuery asks the upstream who the broker logged in as, and whether the
// upstream leg is TLS.
const loginQuery = `select current_user, current_database(), ssl from pg_stat_ssl where pid = pg_backend_pid()`

// login is one case: the database, the user, and the key of the password.
type login struct {
	name, database, user, key string
	run                       func(ctx context.Context, conn *pgx.Conn, o *Outcome) error
}

var all = []login{
	{"pg-login", "app", "app", DBPass, queryRow},
	{"pg-cancel", "app", "app", DBPass, cancelSleep},
	{"pg-other-namespace", "app", "app", cases.OtherNS, nil},
	{"pg-failed", "down", "app", cases.Broken, nil},
	{"pg-wrong-placeholder", "app", "app", cases.APIKey, nil},
	{"pg-unknown-db", "nope", "app", DBPass, nil},
	{"pg-wrong-user", "app", "intruder", DBPass, nil},
	{"pg-upstream-refused", "stale", "app", cases.OldSecret, nil},
}

// Run connects to the proxy at addr once per case, one after the other, and
// returns one outcome per case. ph maps the keys to the placeholders.
func Run(addr string, ph map[string]string) []Outcome {
	out := make([]Outcome, 0, len(all))
	for _, c := range all {
		out = append(out, runCase(addr, c, ph[c.key]))
	}
	return out
}

func runCase(addr string, c login, password string) Outcome {
	o := Outcome{Case: c.name}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(c.user, password),
		Host:     addr,
		Path:     "/" + c.database,
		RawQuery: fmt.Sprintf("sslmode=disable&connect_timeout=%d&application_name=%s", connectTimeout, url.QueryEscape(c.name)),
	}
	cfg, err := pgx.ParseConfig(u.String())
	if err != nil {
		o.Message = "bad test config: " + err.Error()
		return o
	}
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		fill(&o, err)
		return o
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if c.run == nil {
		o.Message = "the login worked, which this case must not"
		return o
	}
	if err := c.run(ctx, conn, &o); err != nil {
		fill(&o, err)
	}
	return o
}

// fill puts the code and message of err into o.
func fill(o *Outcome, err error) {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		o.Code, o.Message = pe.Code, pe.Message
		return
	}
	o.Message = err.Error()
}

func queryRow(ctx context.Context, conn *pgx.Conn, o *Outcome) error {
	var user, db string
	var ssl bool
	if err := conn.QueryRow(ctx, loginQuery).Scan(&user, &db, &ssl); err != nil {
		return err
	}
	o.Row = fmt.Sprintf("%s %s %t", user, db, ssl)
	return nil
}

// cancelSleep starts a 30 s query, cancels it after one second through the
// connection's cancel request, and records how long the query took.
func cancelSleep(ctx context.Context, conn *pgx.Conn, o *Outcome) error {
	start := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := conn.Exec(ctx, "select pg_sleep(30)")
		done <- err
	}()
	time.Sleep(cancelAfter)
	cctx, cancel := context.WithTimeout(ctx, connectTimeout*time.Second)
	defer cancel()
	if err := conn.PgConn().CancelRequest(cctx); err != nil {
		<-done
		return fmt.Errorf("cancel request: %w", err)
	}
	err := <-done
	o.MS = time.Since(start).Milliseconds()
	return err
}
