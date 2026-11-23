package proxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"math/big"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/abrose/sbx-broker/internal/config"
	"github.com/abrose/sbx-broker/internal/eventlog"
	"github.com/abrose/sbx-broker/internal/secrets"
)

const (
	dbPass    = "real-db-pass-1"
	oldDBPass = "old-db-pass-0"
)

// fakePG is a Postgres server for the broker's upstream login. It answers an
// SSLRequest with S and TLS, asks for a cleartext password, and records the
// startup and the password. Then it answers authErr, or the login and an echo
// of each query as a CommandComplete.
type fakePG struct {
	addr    string
	authErr *pgproto3.ErrorResponse
	noTLS   bool // answer an SSLRequest with N and go on in plain text
	// authMode is the login the fake asks for: "" cleartext, "md5", or "none"
	// (AuthenticationOk at once).
	authMode string
	// gone, when set, gets one value each time a connection of the fake ends.
	gone chan struct{}
	// sessionKey, when set, gives the secret key of the nth login (from 1)
	// instead of the same key for every login.
	sessionKey func(n int) []byte

	mu        sync.Mutex
	conns     int
	startups  []map[string]string
	passwords []string
	cancels   []pgproto3.CancelRequest
	open      map[net.Conn]bool
}

func startFakePG(t *testing.T, authErr *pgproto3.ErrorResponse) *fakePG {
	return startFake(t, &fakePG{authErr: authErr})
}

func startFake(t *testing.T, f *fakePG) *fakePG {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.addr, f.open = ln.Addr().String(), map[net.Conn]bool{}
	tlsConf := &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}}
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.conns++
			f.open[c] = true
			f.mu.Unlock()
			wg.Go(func() { f.serve(c, tlsConf) })
		}
	})
	t.Cleanup(func() {
		ln.Close()
		f.mu.Lock()
		for c := range f.open {
			c.Close()
		}
		f.mu.Unlock()
		wg.Wait()
	})
	return f
}

func (f *fakePG) serve(c net.Conn, tlsConf *tls.Config) {
	defer func() {
		c.Close()
		if f.gone != nil {
			f.gone <- struct{}{}
		}
	}()
	var conn net.Conn = c
	be := pgproto3.NewBackend(conn, conn)
	msg, err := be.ReceiveStartupMessage()
	if err != nil {
		return
	}
	switch m := msg.(type) {
	case *pgproto3.CancelRequest:
		f.mu.Lock()
		f.cancels = append(f.cancels, pgproto3.CancelRequest{ProcessID: m.ProcessID, SecretKey: slices.Clone(m.SecretKey)})
		f.mu.Unlock()
		return
	case *pgproto3.SSLRequest:
		if f.noTLS {
			if _, err := c.Write([]byte{'N'}); err != nil {
				return
			}
		} else {
			if _, err := c.Write([]byte{'S'}); err != nil {
				return
			}
			tc := tls.Server(c, tlsConf)
			if err := tc.Handshake(); err != nil {
				return
			}
			conn = tc
			be = pgproto3.NewBackend(conn, conn)
		}
		if msg, err = be.ReceiveStartupMessage(); err != nil {
			return
		}
	}
	st, ok := msg.(*pgproto3.StartupMessage)
	if !ok {
		return
	}
	f.mu.Lock()
	f.startups = append(f.startups, maps.Clone(st.Parameters))
	nth := len(f.startups)
	f.mu.Unlock()
	secretKey := []byte{9, 9, 9, 9}
	if f.sessionKey != nil {
		secretKey = f.sessionKey(nth)
	}
	switch f.authMode {
	case "none": // AuthenticationOk with no authentication at all
		be.Send(&pgproto3.AuthenticationOk{})
		_ = be.Flush()
		return
	case "md5":
		be.Send(&pgproto3.AuthenticationMD5Password{Salt: [4]byte{1, 2, 3, 4}})
		if be.Flush() != nil || be.SetAuthType(pgproto3.AuthTypeMD5Password) != nil {
			return
		}
	default:
		be.Send(&pgproto3.AuthenticationCleartextPassword{})
		if be.Flush() != nil || be.SetAuthType(pgproto3.AuthTypeCleartextPassword) != nil {
			return
		}
	}
	msg, err = be.Receive()
	if err != nil {
		return
	}
	pw, ok := msg.(*pgproto3.PasswordMessage)
	if !ok {
		return
	}
	if f.authMode == "md5" { // the hash is of the password: record it as one
		pw.Password = "md5:" + pw.Password
	}
	f.mu.Lock()
	f.passwords = append(f.passwords, pw.Password)
	f.mu.Unlock()
	if f.authErr != nil {
		be.Send(f.authErr)
		_ = be.Flush()
		return
	}
	be.Send(&pgproto3.AuthenticationOk{})
	be.Send(&pgproto3.ParameterStatus{Name: "server_version", Value: "17.0"})
	be.Send(&pgproto3.ParameterStatus{Name: "client_encoding", Value: "UTF8"})
	be.Send(&pgproto3.BackendKeyData{ProcessID: 42, SecretKey: secretKey})
	be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if be.Flush() != nil {
		return
	}
	for {
		msg, err := be.Receive()
		if err != nil {
			return
		}
		q, ok := msg.(*pgproto3.Query)
		if !ok {
			return
		}
		be.Send(&pgproto3.CommandComplete{CommandTag: []byte(q.String)})
		if be.Flush() != nil {
			return
		}
	}
}

func (f *fakePG) connCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns
}

func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fake-upstream"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// pgSecrets are the values of t1 for the Postgres tests, with a retired
// value of db-pass.
type pgSecrets struct{ fakeSecrets }

func (pgSecrets) Retired() map[string][][]byte {
	return map[string][][]byte{"db-pass": {[]byte(oldDBPass)}}
}

func newPGSecrets() pgSecrets {
	return pgSecrets{fakeSecrets{
		"db-pass": {value: dbPass, state: secrets.StateOK},
		"broken":  {state: secrets.StateFailed},
	}}
}

// pgEntries are the databases of t1: app and down at upstream, plus extra.
func pgEntries(upstream string, extra ...config.Database) config.Postgres {
	return config.Postgres{Databases: append([]config.Database{
		{Alias: "app", Upstream: upstream, DBName: "appdb", User: "app", Password: "db-pass"},
		{Alias: "down", Upstream: upstream, DBName: "appdb", User: "app", Password: "broken"},
	}, extra...)}
}

// newPG makes the listener of t1 on a free port. It does not start it.
func newPG(t *testing.T, pg config.Postgres) (*Postgres, *eventlog.Log) {
	t.Helper()
	l := openLog(t)
	return NewPostgres(ns, pg, newPGSecrets(), l), l
}

func startPG(t *testing.T, p *Postgres) {
	t.Helper()
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
}

// pgApp is the app's end of a connection to the listener.
type pgApp struct {
	t  *testing.T
	c  net.Conn
	fe *pgproto3.Frontend
}

func dialPG(t *testing.T, p *Postgres) *pgApp {
	t.Helper()
	c, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return &pgApp{t: t, c: c, fe: pgproto3.NewFrontend(c, c)}
}

// send writes msgs in one write.
func (a *pgApp) send(msgs ...interface{ Encode([]byte) ([]byte, error) }) {
	a.t.Helper()
	var buf []byte
	for _, m := range msgs {
		var err error
		if buf, err = m.Encode(buf); err != nil {
			a.t.Fatal(err)
		}
	}
	if _, err := a.c.Write(buf); err != nil {
		a.t.Fatal(err)
	}
}

func (a *pgApp) recv() pgproto3.BackendMessage {
	a.t.Helper()
	msg, err := a.fe.Receive()
	if err != nil {
		a.t.Fatalf("receive: %v", err)
	}
	return msg
}

// startup sends a 3.0 startup and reads up to the password request.
func (a *pgApp) startup(user, db string) {
	a.t.Helper()
	a.send(&pgproto3.StartupMessage{ProtocolVersion: pgproto3.ProtocolVersion30,
		Parameters: map[string]string{"user": user, "database": db}})
	a.wantPasswordRequest()
}

func (a *pgApp) wantPasswordRequest() {
	a.t.Helper()
	if msg := a.recv(); !isType[*pgproto3.AuthenticationCleartextPassword](msg) {
		a.t.Fatalf("got %#v, want the password request", msg)
	}
}

// wantFatal reads one ErrorResponse, then EOF.
func (a *pgApp) wantFatal(code, message string) {
	a.t.Helper()
	er, ok := a.recv().(*pgproto3.ErrorResponse)
	if !ok || er.Severity != "FATAL" || er.Code != code || er.Message != message {
		a.t.Fatalf("got %#v, want FATAL %s %s", er, code, message)
	}
	a.wantEOF()
}

func (a *pgApp) wantEOF() {
	a.t.Helper()
	if _, err := a.fe.Receive(); !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		a.t.Fatalf("got %v, want EOF", err)
	}
}

// wantLogin reads the hand-off of the fake's login.
func (a *pgApp) wantLogin() {
	a.t.Helper()
	a.wantLoginKey([]byte{9, 9, 9, 9})
}

// wantLoginKey is wantLogin for a fake that hands out the secret key key.
func (a *pgApp) wantLoginKey(key []byte) {
	a.t.Helper()
	if msg := a.recv(); !isType[*pgproto3.AuthenticationOk](msg) {
		a.t.Fatalf("got %#v, want AuthenticationOk", msg)
	}
	var params []string
	for range 2 {
		ps, ok := a.recv().(*pgproto3.ParameterStatus)
		if !ok {
			a.t.Fatal("want ParameterStatus")
		}
		params = append(params, ps.Name+"="+ps.Value)
	}
	if want := []string{"client_encoding=UTF8", "server_version=17.0"}; !slices.Equal(params, want) {
		a.t.Fatalf("parameters %v, want %v", params, want)
	}
	bk, ok := a.recv().(*pgproto3.BackendKeyData)
	if !ok || bk.ProcessID != 42 || !slices.Equal(bk.SecretKey, key) {
		a.t.Fatalf("got %#v, want BackendKeyData{42, %v}", bk, key)
	}
	rq, ok := a.recv().(*pgproto3.ReadyForQuery)
	if !ok || rq.TxStatus != 'I' {
		a.t.Fatalf("got %#v, want ReadyForQuery{I}", rq)
	}
}

func (a *pgApp) wantCommand(tag string) {
	a.t.Helper()
	cc, ok := a.recv().(*pgproto3.CommandComplete)
	if !ok || string(cc.CommandTag) != tag {
		a.t.Fatalf("got %#v, want CommandComplete %q", cc, tag)
	}
}

func isType[T any](v any) bool { _, ok := v.(T); return ok }

// noDBValueInLog fails when an event field holds a value of db-pass.
func noDBValueInLog(t *testing.T, l *eventlog.Log) {
	t.Helper()
	data, err := json.Marshal(l.Recent(1000))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), dbPass) || strings.Contains(string(data), oldDBPass) {
		t.Error("the event log holds a value of db-pass")
	}
}

func wantEvent(t *testing.T, e eventlog.Event, want eventlog.Event) {
	t.Helper()
	if e.Kind != want.Kind || e.Namespace != ns || e.Route != want.Route || e.User != want.User ||
		e.Direction != "request" || e.Message != want.Message || !slices.Equal(e.Secrets, want.Secrets) {
		t.Errorf("event %+v, want %+v", e, want)
	}
}

func TestPGLoginSwapsPassword(t *testing.T) {
	fake := startFakePG(t, nil)
	p, l := newPG(t, pgEntries(fake.addr))
	startPG(t, p)
	a := dialPG(t, p)
	a.send(&pgproto3.StartupMessage{ProtocolVersion: pgproto3.ProtocolVersion30, Parameters: map[string]string{
		"user": "app", "database": "app", "application_name": "acme-app", "_pq_.acme": "1"}})
	if msg := a.recv(); !isType[*pgproto3.NegotiateProtocolVersion](msg) {
		t.Fatalf("got %#v, want NegotiateProtocolVersion", msg)
	}
	a.wantPasswordRequest()
	a.send(&pgproto3.PasswordMessage{Password: "sbxb_t1_db_pass"})
	a.wantLogin()
	a.send(&pgproto3.Query{String: "select 1"})
	a.wantCommand("select 1")

	fake.mu.Lock()
	passwords, startups := slices.Clone(fake.passwords), slices.Clone(fake.startups)
	fake.mu.Unlock()
	if len(passwords) != 1 || passwords[0] != dbPass {
		t.Error("the fake did not get the real password")
	}
	if len(startups) != 1 {
		t.Fatalf("%d startups", len(startups))
	}
	st := startups[0]
	if st["user"] != "app" || st["database"] != "appdb" || st["application_name"] != "acme-app" {
		t.Errorf("startup %v", st)
	}
	for k := range st {
		if strings.HasPrefix(k, "_pq_.") {
			t.Errorf("startup holds %s", k)
		}
	}
	swaps := eventsOf(l, eventlog.KindSwap)
	if len(swaps) != 1 {
		t.Fatalf("%d swap events", len(swaps))
	}
	wantEvent(t, swaps[0], eventlog.Event{Kind: eventlog.KindSwap, Route: "app", User: "app", Secrets: []string{"db-pass"}, Message: "login"})
	noDBValueInLog(t, l)
}

// The app picks the user and the database of a startup, and Postgres results
// are not scanned: an app that learned a value can put it there. The log's
// redactor must catch it in the route and user fields.
func TestPGAppControlledFieldsAreRedacted(t *testing.T) {
	fake := startFakePG(t, nil)
	p, l := newPG(t, pgEntries(fake.addr))
	l.SetRedactor(func(s string) string { return strings.ReplaceAll(s, dbPass, "[redacted]") })
	startPG(t, p)

	// A known database, so the login goes through: the user is the value.
	a := dialPG(t, p)
	a.startup(dbPass, "app")
	a.send(&pgproto3.PasswordMessage{Password: "sbxb_t1_db_pass"})
	if er, ok := a.recv().(*pgproto3.ErrorResponse); !ok || er.Severity != "FATAL" {
		t.Fatalf("got %#v, want a refusal", er)
	}

	// An unknown database (check 2): the database is the value.
	b := dialPG(t, p)
	b.startup("app", dbPass)
	b.send(&pgproto3.PasswordMessage{Password: "sbxb_t1_db_pass"})
	if er, ok := b.recv().(*pgproto3.ErrorResponse); !ok || er.Severity != "FATAL" {
		t.Fatalf("got %#v, want a refusal", er)
	}

	if n := len(l.Recent(1000)); n == 0 {
		t.Fatal("no events")
	}
	noDBValueInLog(t, l)
}

func TestPGLoginProtocol32Client(t *testing.T) {
	fake := startFakePG(t, nil)
	p, _ := newPG(t, pgEntries(fake.addr))
	startPG(t, p)
	a := dialPG(t, p)
	a.send(&pgproto3.StartupMessage{ProtocolVersion: pgproto3.ProtocolVersion32,
		Parameters: map[string]string{"user": "app", "database": "app"}})
	neg, ok := a.recv().(*pgproto3.NegotiateProtocolVersion)
	if !ok || neg.NewestMinorProtocol != 0 {
		t.Fatalf("got %#v, want NegotiateProtocolVersion 3.0", neg)
	}
	a.wantPasswordRequest()
	a.send(&pgproto3.PasswordMessage{Password: "sbxb_t1_db_pass"})
	a.wantLogin()
	a.send(&pgproto3.Query{String: "select 1"})
	a.wantCommand("select 1")
}

func TestPGNoReadAhead(t *testing.T) {
	fake := startFakePG(t, nil)
	p, _ := newPG(t, pgEntries(fake.addr))
	startPG(t, p)
	a := dialPG(t, p)
	a.startup("app", "app")
	a.send(&pgproto3.PasswordMessage{Password: "sbxb_t1_db_pass"}, &pgproto3.Query{String: "select 2"})
	a.wantLogin()
	a.wantCommand("select 2")
}

func TestPGRefusalsNeverDial(t *testing.T) {
	tests := []struct {
		name, user, db, password string
		code, message            string
		events                   []eventlog.Event
	}{
		{name: "1 other namespace", user: "app", db: "app", password: "sbxb_t2_db_pass",
			code: "28000", message: "placeholder of another namespace",
			events: []eventlog.Event{
				{Kind: eventlog.KindRefuse, Route: "app", User: "app", Message: "28000 placeholder of another namespace"},
				{Kind: eventlog.KindViolation, Route: "app", User: "app", Message: "placeholder of namespace t2"},
			}},
		{name: "2 unknown database", user: "app", db: "nope", password: "sbxb_t1_db_pass",
			code: "3D000", message: `database "nope" is not configured in namespace t1`,
			events: []eventlog.Event{{Kind: eventlog.KindRefuse, Route: "nope", User: "app",
				Message: `3D000 database "nope" is not configured in namespace t1`}}},
		{name: "3 wrong user", user: "root", db: "app", password: "sbxb_t1_db_pass",
			code: "28000", message: `user "root" may not use database "app"`,
			events: []eventlog.Event{{Kind: eventlog.KindRefuse, Route: "app", User: "root",
				Message: `28000 user "root" may not use database "app"`}}},
		{name: "4 not the placeholder", user: "app", db: "app", password: "hunter2",
			code: "28P01", message: `password is not the placeholder of database "app"`,
			events: []eventlog.Event{{Kind: eventlog.KindRefuse, Route: "app", User: "app",
				Message: `28P01 password is not the placeholder of database "app"`}}},
		{name: "5 secret not loaded", user: "app", db: "down", password: "sbxb_t1_broken",
			code: "57P03", message: "secret not loaded: broken (failed)",
			events: []eventlog.Event{{Kind: eventlog.KindRefuse, Route: "down", User: "app", Secrets: []string{"broken"},
				Message: "57P03 secret not loaded: broken (failed)"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := startFakePG(t, nil)
			p, l := newPG(t, pgEntries(fake.addr))
			startPG(t, p)
			a := dialPG(t, p)
			a.startup(tc.user, tc.db)
			a.send(&pgproto3.PasswordMessage{Password: tc.password})
			a.wantFatal(tc.code, tc.message)
			if n := fake.connCount(); n != 0 {
				t.Errorf("the fake got %d connections", n)
			}
			events := l.Recent(1000)
			if len(events) != len(tc.events) {
				t.Fatalf("events %+v, want %+v", events, tc.events)
			}
			for i, want := range tc.events {
				wantEvent(t, events[i], want)
			}
		})
	}
}

func TestPGUpstreamUnreachable(t *testing.T) {
	dead := config.Database{Alias: "dead", Upstream: "127.0.0.1:" + strconv.Itoa(freePort(t)), DBName: "appdb", User: "app", Password: "db-pass"}
	p, l := newPG(t, pgEntries("127.0.0.1:1", dead))
	startPG(t, p)
	a := dialPG(t, p)
	a.startup("app", "dead")
	a.send(&pgproto3.PasswordMessage{Password: "sbxb_t1_db_pass"})
	a.wantFatal("08006", `upstream unreachable: database "dead"`)
	events := l.Recent(1000)
	if len(events) != 1 {
		t.Fatalf("events %+v", events)
	}
	wantEvent(t, events[0], eventlog.Event{Kind: eventlog.KindUpstream, Route: "dead", User: "app",
		Secrets: []string{"db-pass"}, Message: `08006 upstream unreachable: database "dead"`})
}

// TestPGUpstreamWithoutTLS: an upstream that refuses TLS never gets the
// password in plain text.
func TestPGUpstreamWithoutTLS(t *testing.T) {
	fake := startFake(t, &fakePG{noTLS: true})
	p, l := newPG(t, pgEntries(fake.addr))
	startPG(t, p)
	a := dialPG(t, p)
	a.startup("app", "app")
	a.send(&pgproto3.PasswordMessage{Password: "sbxb_t1_db_pass"})
	a.wantFatal("08006", `upstream unreachable: database "app"`)
	fake.mu.Lock()
	startups, passwords := len(fake.startups), len(fake.passwords)
	fake.mu.Unlock()
	if startups != 0 || passwords != 0 {
		t.Errorf("the fake got %d startups and %d passwords in plain text", startups, passwords)
	}
	if ups := eventsOf(l, eventlog.KindUpstream); len(ups) != 1 {
		t.Errorf("%d upstream events", len(ups))
	}
}

// keptSecrets hands out the values of pgSecrets and keeps every copy.
type keptSecrets struct {
	pgSecrets
	mu     sync.Mutex
	copies [][]byte
}

func (k *keptSecrets) keep(vs ...[]byte) {
	k.mu.Lock()
	k.copies = append(k.copies, vs...)
	k.mu.Unlock()
}

func (k *keptSecrets) Lookup(name string) ([]byte, secrets.State) {
	v, st := k.pgSecrets.Lookup(name)
	k.keep(v)
	return v, st
}

func (k *keptSecrets) Values() map[string][]byte {
	vals := k.pgSecrets.Values()
	k.keep(slices.Collect(maps.Values(vals))...)
	return vals
}

func (k *keptSecrets) Retired() map[string][][]byte {
	ret := k.pgSecrets.Retired()
	for _, vs := range ret {
		k.keep(vs...)
	}
	return ret
}

// wiped checks that values were handed out and that each is all zero now.
func (k *keptSecrets) wiped(t *testing.T) {
	t.Helper()
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.copies) == 0 {
		t.Fatal("no value was handed out")
	}
	for i, v := range k.copies {
		if len(v) == 0 || strings.Trim(string(v), "\x00") != "" {
			t.Errorf("copy %d is not wiped", i)
		}
	}
}

func TestPGWipesValues(t *testing.T) {
	login := func(t *testing.T, fake *fakePG) (*pgApp, *keptSecrets, *eventlog.Log) {
		kept := &keptSecrets{pgSecrets: newPGSecrets()}
		l := openLog(t)
		p := NewPostgres(ns, pgEntries(fake.addr), kept, l)
		startPG(t, p)
		a := dialPG(t, p)
		a.startup("app", "app")
		a.send(&pgproto3.PasswordMessage{Password: "sbxb_t1_db_pass"})
		return a, kept, l
	}
	t.Run("after the login", func(t *testing.T) {
		a, kept, l := login(t, startFakePG(t, nil))
		a.wantLogin()
		waitEvent(t, l, eventlog.KindSwap)
		kept.wiped(t)
	})
	t.Run("after an upstream error", func(t *testing.T) {
		a, kept, l := login(t, startFakePG(t, &pgproto3.ErrorResponse{Severity: "FATAL", Code: "28P01", Message: "no"}))
		a.wantFatal("28P01", "upstream: no")
		waitEvent(t, l, eventlog.KindUpstream)
		kept.wiped(t)
	})
}

func TestPGUpstreamErrorScrubbed(t *testing.T) {
	fake := startFakePG(t, &pgproto3.ErrorResponse{Severity: "FATAL", SeverityUnlocalized: "FATAL", Code: "28P01",
		Message: "bad password " + dbPass + ", was " + oldDBPass})
	p, l := newPG(t, pgEntries(fake.addr))
	startPG(t, p)
	a := dialPG(t, p)
	a.startup("app", "app")
	a.send(&pgproto3.PasswordMessage{Password: "sbxb_t1_db_pass"})
	a.wantFatal("28P01", "upstream: bad password sbxb_t1_db_pass, was sbxb_t1_db_pass")
	events := l.Recent(1000)
	if len(events) != 1 {
		t.Fatalf("events %+v", events)
	}
	wantEvent(t, events[0], eventlog.Event{Kind: eventlog.KindUpstream, Route: "app", User: "app",
		Secrets: []string{"db-pass"}, Message: "28P01 upstream: bad password sbxb_t1_db_pass, was sbxb_t1_db_pass"})
	noDBValueInLog(t, l)
}

func TestUpstreamConfigIgnoresPGEnv(t *testing.T) {
	for k, v := range map[string]string{
		"PGSSLMODE": "disable", "PGUSER": "x", "PGDATABASE": "x", "PGOPTIONS": "-c x=1", "PGHOST": "x", "PGPORT": "1",
		"PGPASSWORD": "x", "PGPASSFILE": "/nonexistent", "PGAPPNAME": "x", "PGCONNECT_TIMEOUT": "1",
		"PGSSLROOTCERT": "/nonexistent", "PGSSLCERT": "/nonexistent", "PGSSLKEY": "/nonexistent",
		"PGSSLCERTMODE": "require", "PGSSLNEGOTIATION": "direct", "PGTARGETSESSIONATTRS": "read-write",
		"PGTZ": "x", "PGMINPROTOCOLVERSION": "3.2", "PGMAXPROTOCOLVERSION": "3.2",
		"PGCHANNELBINDING": "require", "PGREQUIREAUTH": "md5",
	} {
		t.Setenv(k, v)
	}
	db := config.Database{Alias: "app", Upstream: "127.0.0.1:6543", DBName: "appdb", User: "app", Password: "db-pass"}
	params := map[string]string{"application_name": "acme-app"}
	cfg, err := upstreamConfig(db, "pw", params)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "127.0.0.1" || cfg.Port != 6543 || cfg.User != "app" || cfg.Database != "appdb" || cfg.Password != "pw" {
		t.Errorf("host %s port %d user %s database %s", cfg.Host, cfg.Port, cfg.User, cfg.Database)
	}
	if cfg.TLSConfig == nil || !cfg.TLSConfig.InsecureSkipVerify || cfg.TLSConfig.VerifyPeerCertificate != nil {
		t.Errorf("TLS config %+v", cfg.TLSConfig)
	}
	if len(cfg.Fallbacks) != 0 {
		t.Errorf("fallbacks %+v", cfg.Fallbacks)
	}
	if !maps.Equal(cfg.RuntimeParams, params) {
		t.Errorf("runtime params %v", cfg.RuntimeParams)
	}
	if cfg.MinProtocolVersion != "3.0" || cfg.MaxProtocolVersion != "3.0" || cfg.ConnectTimeout != 10*time.Second {
		t.Errorf("protocol %s-%s timeout %s", cfg.MinProtocolVersion, cfg.MaxProtocolVersion, cfg.ConnectTimeout)
	}
	if cfg.SSLNegotiation == "direct" || cfg.SSLCertMode != "disable" || cfg.ValidateConnect != nil ||
		cfg.ChannelBinding != "prefer" || cfg.RequireAuth != "" {
		t.Errorf("negotiation %q certmode %q channel binding %q require auth %q validate %v",
			cfg.SSLNegotiation, cfg.SSLCertMode, cfg.ChannelBinding, cfg.RequireAuth, cfg.ValidateConnect != nil)
	}
}

// TestUpstreamConfigFailsWithPGService documents why server.Run removes the
// PG* variables: no connection string turns off pgconn's service lookup
// (an empty service is still a service), so a PGSERVICE that does not resolve
// fails every upstream login.
func TestUpstreamConfigFailsWithPGService(t *testing.T) {
	t.Setenv("PGSERVICE", "acme-missing")
	t.Setenv("PGSERVICEFILE", "/nonexistent/pg_service.conf")
	db := config.Database{Alias: "app", Upstream: "127.0.0.1:6543", DBName: "appdb", User: "app", Password: "db-pass"}
	if _, err := upstreamConfig(db, "pw", nil); err == nil || !strings.Contains(err.Error(), "failed to read service") {
		t.Fatalf("err = %v; if pgconn no longer reads the service here, server.Run need not drop PG*", err)
	}
}

func TestPGLoginTimeoutCloses(t *testing.T) {
	p, l := newPG(t, pgEntries("127.0.0.1:1"))
	p.loginTimeout = 100 * time.Millisecond
	startPG(t, p)
	a := dialPG(t, p)
	// Half a startup: the length of 40 bytes, then 2 of them.
	if _, err := a.c.Write(append(binary.BigEndian.AppendUint32(nil, 40), 0, 3)); err != nil {
		t.Fatal(err)
	}
	if err := a.c.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("got %v, want EOF", err)
	}
	if events := l.Recent(1000); len(events) != 0 {
		t.Fatalf("events %+v", events)
	}
}

func TestPGCloseEndsSessions(t *testing.T) {
	fake := startFakePG(t, nil)
	p, l := newPG(t, pgEntries(fake.addr))
	startPG(t, p)
	a := dialPG(t, p)
	a.startup("app", "app")
	a.send(&pgproto3.PasswordMessage{Password: "sbxb_t1_db_pass"})
	a.wantLogin()
	done := make(chan error, 1)
	go func() { done <- p.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return")
	}
	a.wantEOF()
	closes := eventsOf(l, eventlog.KindClose)
	if len(closes) != 1 {
		t.Fatalf("%d close events", len(closes))
	}
	wantEvent(t, closes[0], eventlog.Event{Kind: eventlog.KindClose, Route: "app", User: "app", Message: "closed"})
	if closes[0].DurationMS < 0 {
		t.Errorf("duration %d", closes[0].DurationMS)
	}
}

func TestPGListensOnLoopbackOnly(t *testing.T) {
	p, _ := newPG(t, pgEntries("127.0.0.1:1"))
	startPG(t, p)
	if !strings.HasPrefix(p.Addr(), "127.0.0.1:") {
		t.Fatalf("addr %s", p.Addr())
	}
}

func TestPGStartFailsOnTakenPort(t *testing.T) {
	p, _ := newPG(t, pgEntries("127.0.0.1:1"))
	startPG(t, p)
	_, port, _ := net.SplitHostPort(p.Addr())
	pg := pgEntries("127.0.0.1:1")
	pg.Listen, _ = strconv.Atoi(port)
	q, _ := newPG(t, pg)
	if err := q.Start(); err == nil || !strings.HasPrefix(err.Error(), "postgres: ") {
		_ = q.Close()
		t.Fatalf("err = %v", err)
	}
}

// TestPGProtocolErrors: a protocol error after the startup is a refusal; one
// before any startup gets the fatal but no event.
func TestPGProtocolErrors(t *testing.T) {
	t.Run("password too long", func(t *testing.T) {
		p, l := newPG(t, pgEntries("127.0.0.1:1"))
		startPG(t, p)
		a := dialPG(t, p)
		a.startup("app", "app")
		if _, err := a.c.Write(binary.BigEndian.AppendUint32([]byte{'p'}, 1001)); err != nil {
			t.Fatal(err)
		}
		a.wantFatal("08P01", "message too long")
		events := l.Recent(1000)
		if len(events) != 1 {
			t.Fatalf("events %+v", events)
		}
		wantEvent(t, events[0], eventlog.Event{Kind: eventlog.KindRefuse, Route: "app", User: "app", Message: "08P01 message too long"})
	})
	t.Run("other major before the startup", func(t *testing.T) {
		p, l := newPG(t, pgEntries("127.0.0.1:1"))
		startPG(t, p)
		a := dialPG(t, p)
		body := binary.BigEndian.AppendUint32(nil, 2<<16)
		body = append(body, "user\x00app\x00\x00"...)
		if _, err := a.c.Write(append(binary.BigEndian.AppendUint32(nil, uint32(len(body)+4)), body...)); err != nil {
			t.Fatal(err)
		}
		er, ok := a.recv().(*pgproto3.ErrorResponse)
		if !ok || er.Code != "08P01" || !strings.Contains(er.Message, "unsupported frontend protocol") {
			t.Fatalf("got %#v", er)
		}
		a.wantEOF()
		if events := l.Recent(1000); len(events) != 0 {
			t.Fatalf("events %+v", events)
		}
	})
}

// TestPGRefusesReplication: a replication connection would stream the WAL of
// the whole cluster, past the entry's dbname and the table grants.
func TestPGRefusesReplication(t *testing.T) {
	fake := startFakePG(t, nil)
	p, l := newPG(t, pgEntries(fake.addr))
	startPG(t, p)
	a := dialPG(t, p)
	a.send(&pgproto3.StartupMessage{ProtocolVersion: pgproto3.ProtocolVersion30,
		Parameters: map[string]string{"user": "app", "database": "app", "replication": "database"}})
	a.wantFatal("0A000", "replication connections are not supported")
	if n := fake.connCount(); n != 0 {
		t.Errorf("the fake got %d connections", n)
	}
	events := l.Recent(1000)
	if len(events) != 1 {
		t.Fatalf("events %+v", events)
	}
	wantEvent(t, events[0], eventlog.Event{Kind: eventlog.KindRefuse, Route: "app", User: "app",
		Message: "0A000 replication connections are not supported"})
}

// cancelFrom opens a new connection to the listener, sends a cancel request,
// and expects the close with no answer. The broker closes only after it has
// dialed and written (or decided not to), so the EOF orders the checks after.
func cancelFrom(t *testing.T, p *Postgres, pid uint32, key []byte) {
	t.Helper()
	a := dialPG(t, p)
	a.send(&pgproto3.CancelRequest{ProcessID: pid, SecretKey: key})
	a.wantEOF()
}

// waitUntil polls cond until it holds or 2 s pass.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (f *fakePG) cancelCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.cancels)
}

// loggedIn starts a listener on fake and returns it with an app past its login.
func loggedIn(t *testing.T, fake *fakePG) (*Postgres, *eventlog.Log, *pgApp) {
	t.Helper()
	p, l := newPG(t, pgEntries(fake.addr))
	startPG(t, p)
	a := dialPG(t, p)
	a.startup("app", "app")
	a.send(&pgproto3.PasswordMessage{Password: "sbxb_t1_db_pass"})
	a.wantLogin()
	return p, l, a
}

func TestPGCancelForwarded(t *testing.T) {
	fake := startFakePG(t, nil)
	p, l, _ := loggedIn(t, fake)
	cancelFrom(t, p, 42, []byte{9, 9, 9, 9})
	waitUntil(t, "the fake records the cancel", func() bool { return fake.cancelCount() == 1 })
	fake.mu.Lock()
	got := fake.cancels[0]
	fake.mu.Unlock()
	if got.ProcessID != 42 || !slices.Equal(got.SecretKey, []byte{9, 9, 9, 9}) {
		t.Errorf("cancel %+v", got)
	}
	if n := fake.connCount(); n != 2 { // the login, and the cancel on its own connection
		t.Errorf("the fake got %d connections", n)
	}
	for _, e := range l.Recent(1000) { // a cancel logs no event
		if e.Kind != eventlog.KindSwap {
			t.Errorf("event %+v", e)
		}
	}
}

// A host name can resolve to several servers: the cancel must go to the one the
// session is connected to, so it is stored as the address of the connection.
func TestPGCancelDialsConnectedAddress(t *testing.T) {
	fake := startFakePG(t, nil)
	_, port, err := net.SplitHostPort(fake.addr)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := newPG(t, pgEntries("localhost:"+port))
	startPG(t, p)
	a := dialPG(t, p)
	a.startup("app", "app")
	a.send(&pgproto3.PasswordMessage{Password: "sbxb_t1_db_pass"})
	a.wantLogin()
	p.mu.Lock()
	var got []string
	for _, s := range p.sessions {
		got = append(got, s.upstream)
	}
	p.mu.Unlock()
	if !slices.Equal(got, []string{fake.addr}) {
		t.Errorf("session upstreams %v, want the connected address %s", got, fake.addr)
	}
	cancelFrom(t, p, 42, []byte{9, 9, 9, 9})
	waitUntil(t, "the fake records the cancel", func() bool { return fake.cancelCount() == 1 })
}

func TestPGCancelUnknownKey(t *testing.T) {
	cases := map[string]struct {
		pid uint32
		key []byte
	}{
		"wrong key":    {42, []byte{1, 1, 1, 1}},
		"wrong pid":    {43, []byte{9, 9, 9, 9}},
		"long key":     {42, []byte{9, 9, 9, 9, 9}},
		"unknown both": {7, []byte{1, 1, 1, 1}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			fake := startFakePG(t, nil)
			p, _, _ := loggedIn(t, fake)
			cancelFrom(t, p, c.pid, c.key)
			// The fake counts a connection in its own goroutine, so a wrongly
			// dialed cancel might not be counted yet. A valid cancel behind it
			// is recorded after the wrong one, and orders the checks.
			cancelFrom(t, p, 42, []byte{9, 9, 9, 9})
			waitUntil(t, "the fake records the valid cancel", func() bool { return fake.cancelCount() == 1 })
			wantOnlyCancel(t, fake, 1, pgproto3.CancelRequest{ProcessID: 42, SecretKey: []byte{9, 9, 9, 9}})
		})
	}
	t.Run("no session", func(t *testing.T) {
		p, l := newPG(t, pgEntries("127.0.0.1:1"))
		startPG(t, p)
		cancelFrom(t, p, 42, []byte{9, 9, 9, 9})
		if events := l.Recent(1000); len(events) != 0 {
			t.Fatalf("events %+v", events)
		}
	})
}

func TestPGCancelAfterSessionEnd(t *testing.T) {
	// Each login gets its own key, so the ended session's key is not the live
	// session's key.
	keyOf := func(n int) []byte { return []byte{9, 9, 9, byte(n)} }
	fake := startFake(t, &fakePG{sessionKey: keyOf})
	p, l := newPG(t, pgEntries(fake.addr))
	startPG(t, p)
	first := dialPG(t, p)
	first.startup("app", "app")
	first.send(&pgproto3.PasswordMessage{Password: "sbxb_t1_db_pass"})
	first.wantLoginKey(keyOf(1))
	_ = first.c.Close()
	// The close event comes after the session leaves the map.
	waitUntil(t, "the session ends", func() bool { return len(eventsOf(l, eventlog.KindClose)) == 1 })
	second := dialPG(t, p)
	second.startup("app", "app")
	second.send(&pgproto3.PasswordMessage{Password: "sbxb_t1_db_pass"})
	second.wantLoginKey(keyOf(2))

	cancelFrom(t, p, 42, keyOf(1)) // the ended session: no dial
	cancelFrom(t, p, 42, keyOf(2)) // the live session: forwarded
	waitUntil(t, "the fake records the valid cancel", func() bool { return fake.cancelCount() == 1 })
	wantOnlyCancel(t, fake, 2, pgproto3.CancelRequest{ProcessID: 42, SecretKey: keyOf(2)})
}

// wantOnlyCancel fails unless the fake got logins login connections and one
// cancel connection, and the cancel is want. Callers wait for the valid cancel
// first, so a wrongly dialed one before it is counted by now.
func wantOnlyCancel(t *testing.T, fake *fakePG, logins int, want pgproto3.CancelRequest) {
	t.Helper()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.conns != logins+1 || len(fake.cancels) != 1 {
		t.Errorf("the fake got %d connections and %d cancels, want %d logins and one cancel", fake.conns, len(fake.cancels), logins)
	}
	if len(fake.cancels) > 0 && (fake.cancels[0].ProcessID != want.ProcessID || !slices.Equal(fake.cancels[0].SecretKey, want.SecretKey)) {
		t.Errorf("cancel %+v, want %+v", fake.cancels[0], want)
	}
}

const scramOnly = "scram-sha-256"

// scramEntries is pgEntries with require-auth on the app entry.
func scramEntries(upstream string) config.Postgres {
	pg := pgEntries(upstream)
	pg.Databases[0].RequireAuth = scramOnly
	return pg
}

func TestUpstreamConfigRequireAuth(t *testing.T) {
	db := config.Database{Alias: "app", Upstream: "127.0.0.1:6543", DBName: "appdb", User: "app", Password: "db-pass"}
	cfg, err := upstreamConfig(db, "pw", nil)
	if err != nil || cfg.RequireAuth != "" {
		t.Fatalf("without the key: %q, %v", cfg.RequireAuth, err)
	}
	db.RequireAuth = scramOnly
	cfg, err = upstreamConfig(db, "pw", nil)
	if err != nil || cfg.RequireAuth != scramOnly {
		t.Fatalf("with the key: %q, %v", cfg.RequireAuth, err)
	}
}

// TestUpstreamRequireAuthErrorText pins the text of pgconn's refusal, which
// the listener matches: a pgx update that changes it must fail here.
func TestUpstreamRequireAuthErrorText(t *testing.T) {
	fake := startFake(t, &fakePG{authMode: "md5"})
	db := config.Database{Alias: "app", Upstream: fake.addr, DBName: "appdb", User: "app", Password: "db-pass", RequireAuth: scramOnly}
	cfg, err := upstreamConfig(db, dbPass, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = pgconn.ConnectConfig(ctx, cfg)
	var pgErr *pgconn.PgError
	if err == nil || errors.As(err, &pgErr) || !strings.Contains(err.Error(), requireAuthFailed) {
		t.Fatalf("err = %v, want a connect error with %q", err, requireAuthFailed)
	}
}

// wantRequireAuthRefusal runs a login against a fake with the given mode and
// checks the answer, the event, and that no password reached the fake.
func wantRequireAuthRefusal(t *testing.T, mode string) {
	t.Helper()
	fake := startFake(t, &fakePG{authMode: mode, gone: make(chan struct{}, 4)})
	p, l := newPG(t, scramEntries(fake.addr))
	startPG(t, p)
	a := dialPG(t, p)
	a.startup("app", "app")
	a.send(&pgproto3.PasswordMessage{Password: "sbxb_t1_db_pass"})
	msg := `upstream: database "app" requires scram-sha-256, the server asked for another method`
	a.wantFatal("28000", msg)
	select { // the fake's side of the login is over
	case <-fake.gone:
	case <-time.After(5 * time.Second):
		t.Fatal("the fake's connection did not end")
	}
	fake.mu.Lock()
	passwords := len(fake.passwords)
	fake.mu.Unlock()
	if passwords != 0 {
		t.Errorf("the fake got %d password messages", passwords)
	}
	events := l.Recent(1000)
	if len(events) != 1 {
		t.Fatalf("events %+v", events)
	}
	wantEvent(t, events[0], eventlog.Event{Kind: eventlog.KindUpstream, Route: "app", User: "app",
		Secrets: []string{"db-pass"}, Message: "28000 " + msg})
	noDBValueInLog(t, l)
}

func TestPGRequireAuthRefusesCleartext(t *testing.T) { wantRequireAuthRefusal(t, "") }
func TestPGRequireAuthRefusesMD5(t *testing.T)       { wantRequireAuthRefusal(t, "md5") }
func TestPGRequireAuthRefusesNoAuth(t *testing.T)    { wantRequireAuthRefusal(t, "none") }

func TestPGNoRequireAuthKeepsCleartext(t *testing.T) {
	fake := startFakePG(t, nil)
	p, _ := newPG(t, pgEntries(fake.addr))
	startPG(t, p)
	a := dialPG(t, p)
	a.startup("app", "app")
	a.send(&pgproto3.PasswordMessage{Password: "sbxb_t1_db_pass"})
	a.wantLogin()
	fake.mu.Lock()
	passwords := slices.Clone(fake.passwords)
	fake.mu.Unlock()
	if len(passwords) != 1 || passwords[0] != dbPass {
		t.Error("the fake did not get the real password")
	}
}
