package proxy

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/abrose/sbx-broker/internal/config"
	"github.com/abrose/sbx-broker/internal/eventlog"
	"github.com/abrose/sbx-broker/internal/secrets"
)

type startupResult struct {
	st     *startup
	cancel *pgproto3.CancelRequest
	err    error
}

// runStartup runs readStartup on the server end of a pipe and hands back the
// app end and the result channel.
func runStartup(t *testing.T) (net.Conn, net.Conn, chan startupResult) {
	t.Helper()
	app, srv := net.Pipe()
	t.Cleanup(func() { app.Close(); srv.Close() })
	res := make(chan startupResult, 1)
	go func() {
		st, cancel, err := readStartup(srv)
		res <- startupResult{st, cancel, err}
	}()
	return app, srv, res
}

func encode(t *testing.T, m interface {
	Encode([]byte) ([]byte, error)
}) []byte {
	t.Helper()
	b, err := m.Encode(nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func awaitStartup(t *testing.T, res chan startupResult) startupResult {
	t.Helper()
	select {
	case r := <-res:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("readStartup did not return")
		return startupResult{}
	}
}

func readN(t *testing.T, c net.Conn, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("read %d bytes: %v", n, err)
	}
	return buf
}

func TestStartupPlain(t *testing.T) {
	app, _, res := runStartup(t)
	params := map[string]string{"user": "app", "database": "app", "application_name": "x"}
	app.Write(encode(t, &pgproto3.StartupMessage{ProtocolVersion: pgproto3.ProtocolVersion30, Parameters: params}))
	r := awaitStartup(t, res)
	if r.err != nil || r.cancel != nil {
		t.Fatalf("err=%v cancel=%v", r.err, r.cancel)
	}
	if len(r.st.Params) != 3 || r.st.Params["user"] != "app" || r.st.Params["database"] != "app" || r.st.Params["application_name"] != "x" {
		t.Fatalf("params %v", r.st.Params)
	}
	if len(r.st.PQ) != 0 {
		t.Fatalf("PQ %v", r.st.PQ)
	}
}

func TestStartupSSLRequestGetsN(t *testing.T) {
	app, _, res := runStartup(t)
	app.Write(encode(t, &pgproto3.SSLRequest{}))
	if got := readN(t, app, 1); got[0] != 'N' {
		t.Fatalf("answer %q", got)
	}
	app.Write(encode(t, &pgproto3.StartupMessage{ProtocolVersion: pgproto3.ProtocolVersion30, Parameters: map[string]string{"user": "app"}}))
	if r := awaitStartup(t, res); r.err != nil || r.st.Params["user"] != "app" {
		t.Fatalf("err=%v st=%v", r.err, r.st)
	}
}

func TestStartupSSLAndGSSCountTogether(t *testing.T) {
	app, _, res := runStartup(t)
	app.Write(encode(t, &pgproto3.GSSEncRequest{}))
	readN(t, app, 1)
	app.Write(encode(t, &pgproto3.SSLRequest{}))
	readN(t, app, 1)
	app.Write(encode(t, &pgproto3.SSLRequest{}))
	if r := awaitStartup(t, res); !errors.Is(r.err, errProtocol) {
		t.Fatalf("err=%v", r.err)
	}
}

func TestStartupNegotiates30(t *testing.T) {
	app, _, res := runStartup(t)
	app.Write(encode(t, &pgproto3.StartupMessage{ProtocolVersion: pgproto3.ProtocolVersion32,
		Parameters: map[string]string{"user": "app", "_pq_.b": "1", "_pq_.a": "1"}}))
	frontend := pgproto3.NewFrontend(app, io.Discard)
	app.SetReadDeadline(time.Now().Add(2 * time.Second))
	msg, err := frontend.Receive()
	if err != nil {
		t.Fatal(err)
	}
	neg, ok := msg.(*pgproto3.NegotiateProtocolVersion)
	if !ok || neg.NewestMinorProtocol != 0 || len(neg.UnrecognizedOptions) != 2 ||
		neg.UnrecognizedOptions[0] != "_pq_.a" || neg.UnrecognizedOptions[1] != "_pq_.b" {
		t.Fatalf("got %#v", msg)
	}
	r := awaitStartup(t, res)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if len(r.st.PQ) != 2 || r.st.PQ[0] != "_pq_.a" || r.st.PQ[1] != "_pq_.b" {
		t.Fatalf("PQ %v", r.st.PQ)
	}
	if len(r.st.Params) != 1 || r.st.Params["user"] != "app" {
		t.Fatalf("params %v", r.st.Params)
	}
}

func TestStartupOtherMajor(t *testing.T) {
	app, _, res := runStartup(t)
	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body, 2<<16)
	body = append(body, "user\x00app\x00\x00"...)
	msg := binary.BigEndian.AppendUint32(nil, uint32(len(body)+4))
	app.Write(append(msg, body...))
	if r := awaitStartup(t, res); !errors.Is(r.err, errProtocol) {
		t.Fatalf("err=%v", r.err)
	}
}

func TestStartupCancel(t *testing.T) {
	app, _, res := runStartup(t)
	app.Write(encode(t, &pgproto3.CancelRequest{ProcessID: 7, SecretKey: []byte{1, 2, 3, 4}}))
	r := awaitStartup(t, res)
	if r.err != nil || r.st != nil || r.cancel == nil {
		t.Fatalf("err=%v st=%v cancel=%v", r.err, r.st, r.cancel)
	}
	if r.cancel.ProcessID != 7 || string(r.cancel.SecretKey) != "\x01\x02\x03\x04" {
		t.Fatalf("cancel %+v", r.cancel)
	}
}

func TestStartupTooLong(t *testing.T) {
	app, _, res := runStartup(t)
	// Only the 4 length bytes are written: a reader that wants more blocks
	// and the pipe write below would never finish.
	app.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := app.Write(binary.BigEndian.AppendUint32(nil, 10001)); err != nil {
		t.Fatal(err)
	}
	if r := awaitStartup(t, res); !errors.Is(r.err, errTooLong) {
		t.Fatalf("err=%v", r.err)
	}
}

func TestStartupNoReadAhead(t *testing.T) {
	app, srv, res := runStartup(t)
	msg := encode(t, &pgproto3.StartupMessage{ProtocolVersion: pgproto3.ProtocolVersion30, Parameters: map[string]string{"user": "app"}})
	go app.Write(append(msg, "abcde"...))
	if r := awaitStartup(t, res); r.err != nil {
		t.Fatal(r.err)
	}
	srv.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, 5)
	if _, err := io.ReadFull(srv, got); err != nil || string(got) != "abcde" {
		t.Fatalf("rest %q err=%v", got, err)
	}
}

func TestStartupTimeout(t *testing.T) {
	_, srv, res := runStartup(t)
	srv.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	r := awaitStartup(t, res)
	if !errors.Is(r.err, os.ErrDeadlineExceeded) {
		t.Fatalf("err=%v", r.err)
	}
}

func TestPasswordRead(t *testing.T) {
	app, srv := net.Pipe()
	t.Cleanup(func() { app.Close(); srv.Close() })
	type result struct {
		pw  string
		err error
	}
	res := make(chan result, 1)
	go func() {
		pw, err := readPassword(srv)
		res <- result{pw, err}
	}()
	frontend := pgproto3.NewFrontend(app, io.Discard)
	app.SetReadDeadline(time.Now().Add(2 * time.Second))
	msg, err := frontend.Receive()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := msg.(*pgproto3.AuthenticationCleartextPassword); !ok {
		t.Fatalf("got %#v", msg)
	}
	app.Write(encode(t, &pgproto3.PasswordMessage{Password: "sbxb_t1_db_pass"}))
	r := <-res
	if r.err != nil || r.pw != "sbxb_t1_db_pass" {
		t.Fatalf("pw=%q err=%v", r.pw, r.err)
	}
}

func TestPasswordTooLong(t *testing.T) {
	app, srv := net.Pipe()
	t.Cleanup(func() { app.Close(); srv.Close() })
	res := make(chan error, 1)
	go func() {
		_, err := readPassword(srv)
		res <- err
	}()
	frontend := pgproto3.NewFrontend(app, io.Discard)
	app.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := frontend.Receive(); err != nil {
		t.Fatal(err)
	}
	// Type and length only: the reader must refuse before it reads the body.
	app.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := app.Write(binary.BigEndian.AppendUint32([]byte{'p'}, 1001)); err != nil {
		t.Fatal(err)
	}
	if err := <-res; !errors.Is(err, errTooLong) {
		t.Fatalf("err=%v", err)
	}
}

func TestCheckLogin(t *testing.T) {
	sec := fakeSecrets{
		"db-pass": {value: "real-db-pass-1", state: secrets.StateOK},
		"broken":  {state: secrets.StateFailed},
	}
	pg := config.Postgres{Databases: []config.Database{
		{Alias: "app", Upstream: "<host>:5432", DBName: "appdb", User: "app", Password: "db-pass"},
		{Alias: "down", Upstream: "<host>:5432", DBName: "appdb", User: "app", Password: "broken"},
		{Alias: "gone", Upstream: "<host>:5432", DBName: "appdb", User: "app", Password: "nowhere"},
	}}
	tests := []struct {
		name, user, db, password string
		code, message, secret    string
		kind                     eventlog.Kind
		otherNS                  string
	}{
		{name: "other namespace", user: "app", db: "app", password: "sbxb_t2_db_pass",
			code: "28000", message: "placeholder of another namespace", kind: eventlog.KindRefuse, otherNS: "t2"},
		{name: "other namespace first", user: "app", db: "nope", password: "x sbxb_t2_db_pass",
			code: "28000", message: "placeholder of another namespace", kind: eventlog.KindRefuse, otherNS: "t2"},
		{name: "unknown database", user: "app", db: "nope", password: "sbxb_t1_db_pass",
			code: "3D000", message: `database "nope" is not configured in namespace t1`, kind: eventlog.KindRefuse},
		{name: "wrong user", user: "root", db: "app", password: "sbxb_t1_db_pass",
			code: "28000", message: `user "root" may not use database "app"`, kind: eventlog.KindRefuse},
		{name: "not a placeholder", user: "app", db: "app", password: "hunter2",
			code: "28P01", message: `password is not the placeholder of database "app"`, kind: eventlog.KindRefuse},
		{name: "placeholder with text around it", user: "app", db: "app", password: "xsbxb_t1_db_pass",
			code: "28P01", message: `password is not the placeholder of database "app"`, kind: eventlog.KindRefuse},
		{name: "placeholder of another secret", user: "app", db: "app", password: "sbxb_t1_broken",
			code: "28P01", message: `password is not the placeholder of database "app"`, kind: eventlog.KindRefuse},
		{name: "failed secret", user: "app", db: "down", password: "sbxb_t1_broken",
			code: "57P03", message: "secret not loaded: broken (failed)", kind: eventlog.KindRefuse, secret: "broken"},
		{name: "undefined secret", user: "app", db: "gone", password: "sbxb_t1_nowhere",
			code: "57P03", message: "secret not loaded: nowhere (undefined)", kind: eventlog.KindRefuse, secret: "nowhere"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entry, val, ref := checkLogin("t1", pg, sec, tc.user, tc.db, tc.password)
			if ref == nil {
				t.Fatalf("no refusal, entry %v", entry)
			}
			if val != nil {
				t.Fatalf("value on refusal: %q", val)
			}
			if ref.Code != tc.code || ref.Message != tc.message || ref.Kind != tc.kind ||
				ref.Secret != tc.secret || ref.OtherNS != tc.otherNS {
				t.Fatalf("got %+v", *ref)
			}
		})
	}

	t.Run("valid login", func(t *testing.T) {
		entry, val, ref := checkLogin("t1", pg, sec, "app", "app", "sbxb_t1_db_pass")
		if ref != nil {
			t.Fatalf("refusal %+v", *ref)
		}
		if entry.Alias != "app" || string(val) != "real-db-pass-1" {
			t.Fatalf("entry %v value %q", entry, val)
		}
	})
}

func TestWriteFatal(t *testing.T) {
	app, srv := net.Pipe()
	t.Cleanup(func() { app.Close(); srv.Close() })
	go writeFatal(srv, "28000", "no way")
	frontend := pgproto3.NewFrontend(app, io.Discard)
	app.SetReadDeadline(time.Now().Add(2 * time.Second))
	msg, err := frontend.Receive()
	if err != nil {
		t.Fatal(err)
	}
	er, ok := msg.(*pgproto3.ErrorResponse)
	if !ok || er.Severity != "FATAL" || er.Code != "28000" || er.Message != "no way" {
		t.Fatalf("got %#v", msg)
	}
}

// TestStartupRefusesEmptyParameterName: PostgreSQL ends the list at an empty
// name and wants that NUL to be the last byte. pgproto3 accepts more, and
// Params goes on to the upstream.
func TestStartupRefusesEmptyParameterName(t *testing.T) {
	for name, params := range map[string]string{
		"empty pair":          "user\x00app\x00\x00\x00\x00",
		"pairs after the end": "user\x00app\x00\x00junk\x00\x00",
	} {
		t.Run(name, func(t *testing.T) {
			app, _, res := runStartup(t)
			body := binary.BigEndian.AppendUint32(nil, pgproto3.ProtocolVersion30)
			body = append(body, params...)
			go app.Write(append(binary.BigEndian.AppendUint32(nil, uint32(len(body)+4)), body...))
			if r := awaitStartup(t, res); !errors.Is(r.err, errProtocol) {
				t.Fatalf("err=%v st=%+v", r.err, r.st)
			}
		})
	}
}
