package proxy

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/abrose/sbx-broker/internal/cloexec"
	"github.com/abrose/sbx-broker/internal/config"
	"github.com/abrose/sbx-broker/internal/eventlog"
	"github.com/abrose/sbx-broker/internal/placeholder"
	"github.com/abrose/sbx-broker/internal/transform"
)

// The Postgres listener takes an app's login with a placeholder as the
// password, logs in to the entry's upstream with the real value, hands the
// upstream's session state to the app, and then copies bytes both ways.

// requireAuthFailed is the text of pgconn's refusal of a login method that
// the entry's require-auth does not allow. TestUpstreamRequireAuthErrorText
// pins it.
const requireAuthFailed = "require_auth check failed"

const (
	pgLoginTimeout = 10 * time.Second
	pgDialTimeout  = 10 * time.Second
	acceptBackoff  = 50 * time.Millisecond // after an accept error such as EMFILE

	// upstreamSettings is the keyword string of the upstream login. It
	// gives every setting that a PG* variable or a file in the home folder
	// (.pgpass, .postgresql/*) could set, so none of them applies.
	// upstreamConfig then sets the fields of the entry by hand. The string
	// cannot turn off every PG* variable (PGSERVICE can still make
	// ParseConfig fail, see TestUpstreamConfigFailsWithPGService): that
	// guarantee rests on server.Run's dropPGEnv, which removes every PG*
	// variable from the process before any listener starts.
	upstreamSettings = "host=127.0.0.1 port=5432 user=broker dbname=broker passfile='' " +
		"sslmode=require sslrootcert='' sslcert='' sslkey='' sslcertmode=disable sslpassword='' " +
		"sslsni=1 sslnegotiation=postgres connect_timeout=10 target_session_attrs=any " +
		"min_protocol_version=3.0 max_protocol_version=3.0 channel_binding=prefer require_auth=''"
)

// Postgres is the Postgres listener of one namespace.
type Postgres struct {
	ns           string
	pg           config.Postgres
	sec          transform.Secrets
	log          *eventlog.Log
	loginTimeout time.Duration // from the accept to the end of the password
	dialTimeout  time.Duration // the upstream login

	ctx  context.Context // ends at Close: stops the upstream logins
	stop context.CancelFunc
	wg   sync.WaitGroup // the handlers

	mu       sync.Mutex
	ln       net.Listener
	addr     string
	closed   bool
	conns    map[net.Conn]bool // every open connection: apps, and upstreams of sessions
	sessions map[pgKey]*pgSession
}

// pgKey is the cancel key the app got: the upstream's PID and secret key.
type pgKey struct {
	pid uint32
	key string
}

// pgSession is a login that reached the hand-off. upstream is the entry's
// host:port, where a cancel request goes.
type pgSession struct {
	app, up  net.Conn
	upstream string
}

// pgLogin is what the app said it is, for the events.
type pgLogin struct{ user, db string }

// NewPostgres returns the Postgres listener of namespace ns.
func NewPostgres(ns string, pg config.Postgres, sec transform.Secrets, log *eventlog.Log) *Postgres {
	ctx, stop := context.WithCancel(context.Background())
	return &Postgres{ns: ns, pg: pg, sec: sec, log: log, loginTimeout: pgLoginTimeout, dialTimeout: pgDialTimeout,
		ctx: ctx, stop: stop, conns: map[net.Conn]bool{}, sessions: map[pgKey]*pgSession{}}
}

// Start listens on 127.0.0.1:<pg.Listen> and serves in the background.
func (p *Postgres) Start() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln != nil || p.closed {
		return errors.New("postgres: already started")
	}
	raw, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p.pg.Listen)))
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	// As the HTTP listener: a fetch command must not inherit a connection.
	ln, err := cloexec.Listener(raw)
	if err != nil {
		_ = raw.Close()
		return fmt.Errorf("postgres: %w", err)
	}
	p.ln, p.addr = ln, ln.Addr().String()
	go p.accept(ln)
	return nil
}

// Addr returns the address the listener listens on, once started.
func (p *Postgres) Addr() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.addr
}

// Close stops accepting, closes every open connection (a session's two, and
// each app still in its login), and waits up to 2 s for the handlers. Each
// ended session writes its close event on the way out.
func (p *Postgres) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	ln := p.ln
	conns := slices.Collect(maps.Keys(p.conns))
	p.mu.Unlock()
	p.stop()
	var err error
	if ln != nil {
		err = ln.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	timer := time.NewTimer(handlerWait)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
	return err
}

func (p *Postgres) accept(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			p.add(eventlog.Event{Kind: eventlog.KindError, Message: "postgres: " + err.Error()})
			time.Sleep(acceptBackoff)
			continue
		}
		if !p.track(c, true) {
			_ = c.Close()
			continue
		}
		go p.serve(c)
	}
}

// track adds c to the open connections, and a handler when handler is set,
// unless Close ran.
func (p *Postgres) track(c net.Conn, handler bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.conns[c] = true
	if handler {
		p.wg.Add(1)
	}
	return true
}

func (p *Postgres) untrack(c net.Conn) {
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
	_ = c.Close()
}

// serve runs the login of one app connection, then its session.
func (p *Postgres) serve(app net.Conn) {
	defer p.wg.Done()
	defer p.untrack(app)
	_ = app.SetDeadline(time.Now().Add(p.loginTimeout))
	st, cancel, err := readStartup(app)
	if err != nil {
		p.protocolError(app, nil, err)
		return
	}
	if cancel != nil {
		p.forwardCancel(cancel)
		return
	}
	l := pgLogin{user: st.Params["user"], db: st.Params["database"]}
	if l.db == "" {
		l.db = l.user // as PostgreSQL does
	}
	// A replication connection streams the WAL of the whole cluster, past
	// the entry's dbname and the table grants.
	if _, ok := st.Params["replication"]; ok {
		p.refuse(app, l, &pgRefusal{Code: "0A000", Message: "replication connections are not supported", Kind: eventlog.KindRefuse})
		return
	}
	password, err := readPassword(app)
	if err != nil {
		p.protocolError(app, &l, err)
		return
	}
	_ = app.SetDeadline(time.Time{})
	entry, value, ref := checkLogin(p.ns, p.pg, p.sec, l.user, l.db, password)
	if ref != nil {
		p.refuse(app, l, ref)
		return
	}
	hj, ref := p.login(entry, value, st.Params)
	if ref != nil {
		p.refuse(app, l, ref)
		return
	}
	p.session(app, hj, l, entry)
}

// forwardCancel sends cr to the upstream of the open session that has its PID
// and key, on a new plain TCP connection. With no such session it dials
// nothing. The caller closes the app's connection with no answer either way.
// The path logs no event, writes no value and holds no secret.
func (p *Postgres) forwardCancel(cr *pgproto3.CancelRequest) {
	upstream, ok := p.sessionUpstream(cr)
	if !ok {
		return
	}
	buf, err := cr.Encode(nil)
	if err != nil {
		return
	}
	ctx, stop := context.WithTimeout(p.ctx, p.dialTimeout)
	defer stop()
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", upstream)
	if err != nil {
		return
	}
	if !p.track(c, false) { // Close ran
		_ = c.Close()
		return
	}
	defer p.untrack(c)
	_ = c.SetWriteDeadline(time.Now().Add(p.dialTimeout))
	_, _ = c.Write(buf)
}

// sessionUpstream finds the open session of cr. The key is the only secret of
// a cancel, so it is compared in constant time against every session of the
// PID.
func (p *Postgres) sessionUpstream(cr *pgproto3.CancelRequest) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, s := range p.sessions {
		if k.pid == cr.ProcessID && subtle.ConstantTimeCompare([]byte(k.key), cr.SecretKey) == 1 {
			return s.upstream, true
		}
	}
	return "", false
}

// protocolError answers a broken login with 08P01. A plain read error (the
// app left, the login timed out) gets no answer. Only a login whose startup
// was read (l set) logs a refusal.
func (p *Postgres) protocolError(app net.Conn, l *pgLogin, err error) {
	var msg string
	switch {
	case errors.Is(err, errTooLong):
		msg = errTooLong.Error()
	case errors.Is(err, errProtocol):
		msg = err.Error() // the broker's own text, never the app's bytes
	default:
		return
	}
	if l != nil {
		p.add(l.event(eventlog.KindRefuse, "08P01 "+msg))
	}
	_ = writeFatal(app, "08P01", msg)
}

// refuse logs ref and answers it with a FATAL.
func (p *Postgres) refuse(app net.Conn, l pgLogin, ref *pgRefusal) {
	e := l.event(ref.Kind, ref.Code+" "+ref.Message)
	if ref.Secret != "" {
		e.Secrets = []string{ref.Secret}
	}
	p.add(e)
	if ref.OtherNS != "" {
		p.add(l.event(eventlog.KindViolation, "placeholder of namespace "+ref.OtherNS))
	}
	_ = writeFatal(app, ref.Code, ref.Message)
}

// login logs in to the entry's upstream with value, then wipes value. On
// success the connection is idle and tracked, and the caller owns it.
func (p *Postgres) login(entry config.Database, value []byte, params map[string]string) (*pgconn.HijackedConn, *pgRefusal) {
	defer clear(value)
	unreachable := &pgRefusal{Code: "08006", Message: fmt.Sprintf("upstream unreachable: database %q", entry.Alias),
		Kind: eventlog.KindUpstream, Secret: entry.Password}
	runtime := maps.Clone(params)
	delete(runtime, "user")
	delete(runtime, "database")
	cfg, err := upstreamConfig(entry, string(value), runtime)
	if err != nil {
		return nil, unreachable
	}
	ctx, cancel := context.WithTimeout(p.ctx, p.dialTimeout)
	defer cancel()
	conn, err := pgconn.ConnectConfig(ctx, cfg)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			return nil, &pgRefusal{Code: pgErr.Code, Message: "upstream: " + p.scrub(pgErr.Message),
				Kind: eventlog.KindUpstream, Secret: entry.Password}
		}
		// pgconn gives no sentinel for this refusal, so match its text.
		// Any other error stays 08006: it is fail-closed too.
		if entry.RequireAuth != "" && strings.Contains(err.Error(), requireAuthFailed) {
			return nil, &pgRefusal{Code: "28000", Kind: eventlog.KindUpstream, Secret: entry.Password,
				Message: fmt.Sprintf("upstream: database %q requires %s, the server asked for another method",
					entry.Alias, entry.RequireAuth)}
		}
		return nil, unreachable
	}
	if err := conn.SyncConn(ctx); err != nil {
		_ = conn.Close(ctx)
		return nil, unreachable
	}
	hj, err := conn.Hijack()
	if err != nil {
		_ = conn.Close(ctx)
		return nil, unreachable
	}
	_ = hj.Conn.SetDeadline(time.Time{})
	if !p.track(hj.Conn, false) { // Close ran: as if it cut the login
		_ = hj.Conn.Close()
		return nil, unreachable
	}
	return hj, nil
}

// upstreamConfig is the login to db's upstream with password. params are
// the app's startup parameters without user, database and _pq_.*. The result
// does not depend on the broker's environment because server.Run's dropPGEnv
// removes every PG* variable from the process before any listener starts.
func upstreamConfig(db config.Database, password string, params map[string]string) (*pgconn.Config, error) {
	host, portText, err := net.SplitHostPort(db.Upstream)
	if err != nil {
		return nil, err
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return nil, err
	}
	cfg, err := pgconn.ParseConfig(upstreamSettings)
	if err != nil {
		return nil, err
	}
	cfg.Host, cfg.Port = host, uint16(port)
	cfg.User, cfg.Database, cfg.Password = db.User, db.DBName, password
	// sslmode=require: TLS without a certificate check, and no plaintext
	// fallback. SNI for a host name, as libpq does.
	cfg.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	if net.ParseIP(host) == nil {
		cfg.TLSConfig.ServerName = host
	}
	cfg.Fallbacks = nil
	cfg.RuntimeParams = params
	cfg.MinProtocolVersion, cfg.MaxProtocolVersion = "3.0", "3.0"
	cfg.SSLCertMode, cfg.SSLNegotiation = "disable", "postgres"
	cfg.ValidateConnect = nil
	cfg.ChannelBinding, cfg.RequireAuth = "prefer", db.RequireAuth
	return cfg, nil
}

// scrub replaces each value of the namespace, current and retired, in s
// with its placeholder, then wipes the copies of the values.
func (p *Postgres) scrub(s string) string {
	current, retired := p.sec.Values(), p.sec.Retired()
	patterns := map[string]string{}
	for name, v := range current {
		if len(v) > 0 {
			patterns[string(v)] = placeholder.Format(p.ns, name)
		}
		clear(v)
	}
	for name, vs := range retired {
		for _, v := range vs {
			if len(v) > 0 {
				patterns[string(v)] = placeholder.Format(p.ns, name)
			}
			clear(v)
		}
	}
	r, _ := transform.NewScrubReader(strings.NewReader(s), patterns)
	out, err := io.ReadAll(r)
	if err != nil {
		return "(message withheld)"
	}
	return string(out)
}

// session hands the upstream's session state to the app, then copies bytes
// both ways until one side closes. login tracked the upstream connection.
func (p *Postgres) session(app net.Conn, hj *pgconn.HijackedConn, l pgLogin, entry config.Database) {
	up := hj.Conn
	defer p.untrack(up)
	key := pgKey{pid: hj.PID, key: string(hj.SecretKey)}
	// The address of the connection, not the entry's: a host name can resolve
	// to several servers, and the cancel must reach the one of this session.
	s := &pgSession{app: app, up: up, upstream: up.RemoteAddr().String()}
	p.mu.Lock()
	p.sessions[key] = s
	p.mu.Unlock()
	e := l.event(eventlog.KindSwap, "login")
	e.Secrets = []string{entry.Password}
	p.add(e)
	start := time.Now()
	if err := writeHandoff(app, hj); err == nil {
		pipe(app, up)
	}
	// Out of the map before the close event, so a cancel after the event
	// finds nothing. No path between the insert and here returns early.
	p.mu.Lock()
	if p.sessions[key] == s {
		delete(p.sessions, key)
	}
	p.mu.Unlock()
	e = l.event(eventlog.KindClose, "closed")
	e.DurationMS = time.Since(start).Milliseconds()
	p.add(e)
}

// writeHandoff sends the end of the login to the app, in one write.
func writeHandoff(w io.Writer, hj *pgconn.HijackedConn) error {
	msgs := []pgproto3.BackendMessage{&pgproto3.AuthenticationOk{}}
	for _, name := range slices.Sorted(maps.Keys(hj.ParameterStatuses)) {
		msgs = append(msgs, &pgproto3.ParameterStatus{Name: name, Value: hj.ParameterStatuses[name]})
	}
	msgs = append(msgs, &pgproto3.BackendKeyData{ProcessID: hj.PID, SecretKey: hj.SecretKey},
		&pgproto3.ReadyForQuery{TxStatus: hj.TxStatus})
	var buf []byte
	for _, m := range msgs {
		var err error
		if buf, err = m.Encode(buf); err != nil {
			return err
		}
	}
	_, err := w.Write(buf)
	return err
}

// pipe copies bytes both ways until one side ends, then closes both.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
	_ = a.Close()
	_ = b.Close()
	<-done
}

func (l pgLogin) event(kind eventlog.Kind, msg string) eventlog.Event {
	return eventlog.Event{Kind: kind, Route: l.db, User: l.user, Direction: "request", Message: msg}
}

// add records e with the namespace of p.
func (p *Postgres) add(e eventlog.Event) {
	if p.log == nil {
		return
	}
	e.Namespace = p.ns
	p.log.Add(e)
}
