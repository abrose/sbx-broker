package proxy

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/abrose/sbx-broker/internal/config"
	"github.com/abrose/sbx-broker/internal/eventlog"
	"github.com/abrose/sbx-broker/internal/placeholder"
	"github.com/abrose/sbx-broker/internal/secrets"
	"github.com/abrose/sbx-broker/internal/transform"
)

// The app's side of a Postgres login. Every read takes exactly the length of
// the message from the raw connection, with no buffered reader: a byte the
// app sends after its password must still be on the connection when the
// copy to the upstream starts.

const (
	maxStartupLen  = 10000 // a startup message over this is refused
	maxPasswordLen = 1000  // a password message over this is refused
	maxSSLRequests = 2     // SSLRequest and GSSENCRequest together

	// The request codes of pgproto3 are not exported.
	cancelRequestCode = 80877102
	sslRequestCode    = 80877103
	gssEncRequestCode = 80877104

	pqPrefix = "_pq_."
)

var (
	// errTooLong and errProtocol are the 08P01 cases. The caller writes the
	// fatal; errors.Is tells them from a plain read error.
	errTooLong  = errors.New("message too long")
	errProtocol = errors.New("protocol violation")
)

// startup is the app's startup message: the parameters without the _pq_.
// ones, and the _pq_. names, sorted.
type startup struct {
	Params map[string]string
	PQ     []string
}

// readFrame reads a length field and then exactly the rest of the message.
// The length counts itself. A length over limit fails before the body is read.
func readFrame(c io.Reader, limit uint32) ([]byte, error) {
	var head [4]byte
	if _, err := io.ReadFull(c, head[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(head[:])
	if n > limit {
		return nil, errTooLong
	}
	if n < 4 {
		return nil, fmt.Errorf("%w: bad length %d", errProtocol, n)
	}
	body := make([]byte, n-4)
	if _, err := io.ReadFull(c, body); err != nil {
		return nil, err
	}
	return body, nil
}

// readStartup reads the first message of a connection. An SSLRequest or
// GSSENCRequest gets N, at most twice. A cancel request is the second result.
// Otherwise the startup is the first, and a startup that asks for more than
// protocol 3.0 gets a NegotiateProtocolVersion. Exactly one result is set on
// success.
func readStartup(c io.ReadWriter) (*startup, *pgproto3.CancelRequest, error) {
	for requests := 0; ; {
		body, err := readFrame(c, maxStartupLen)
		if err != nil {
			return nil, nil, err
		}
		if len(body) < 4 {
			return nil, nil, fmt.Errorf("%w: startup too short", errProtocol)
		}
		code := binary.BigEndian.Uint32(body)
		switch code {
		case sslRequestCode, gssEncRequestCode:
			if requests++; requests > maxSSLRequests {
				return nil, nil, fmt.Errorf("%w: too many encryption requests", errProtocol)
			}
			if _, err := c.Write([]byte{'N'}); err != nil {
				return nil, nil, err
			}
		case cancelRequestCode:
			var cr pgproto3.CancelRequest
			if err := cr.Decode(body); err != nil {
				return nil, nil, fmt.Errorf("%w: %v", errProtocol, err)
			}
			return nil, &cr, nil
		default:
			st, err := parseStartup(c, code, body)
			return st, nil, err
		}
	}
}

// parseStartup decodes a startup message of protocol version code and sends
// the negotiation when the app asked for more than 3.0.
func parseStartup(c io.Writer, code uint32, body []byte) (*startup, error) {
	if major := code >> 16; major != 3 {
		return nil, fmt.Errorf("%w: unsupported frontend protocol", errProtocol)
	}
	// pgproto3 decodes only 3.0 and 3.2; any 3.x is read as 3.0 here.
	binary.BigEndian.PutUint32(body, pgproto3.ProtocolVersion30)
	var msg pgproto3.StartupMessage
	if err := msg.Decode(body); err != nil {
		return nil, fmt.Errorf("%w: %v", errProtocol, err)
	}
	st := &startup{Params: map[string]string{}}
	for k, v := range msg.Parameters {
		if strings.HasPrefix(k, pqPrefix) {
			st.PQ = append(st.PQ, k)
			continue
		}
		st.Params[k] = v
	}
	slices.Sort(st.PQ)
	if code&0xffff > 0 || len(st.PQ) > 0 {
		neg := &pgproto3.NegotiateProtocolVersion{NewestMinorProtocol: 0, UnrecognizedOptions: st.PQ}
		buf, err := neg.Encode(nil)
		if err != nil {
			return nil, err
		}
		if _, err := c.Write(buf); err != nil {
			return nil, err
		}
	}
	return st, nil
}

// readPassword asks for a cleartext password and reads one PasswordMessage.
func readPassword(c io.ReadWriter) (string, error) {
	req, err := (&pgproto3.AuthenticationCleartextPassword{}).Encode(nil)
	if err != nil {
		return "", err
	}
	if _, err := c.Write(req); err != nil {
		return "", err
	}
	var kind [1]byte
	if _, err := io.ReadFull(c, kind[:]); err != nil {
		return "", err
	}
	if kind[0] != 'p' {
		return "", fmt.Errorf("%w: expected a password message", errProtocol)
	}
	body, err := readFrame(c, maxPasswordLen)
	if err != nil {
		return "", err
	}
	var msg pgproto3.PasswordMessage
	if err := msg.Decode(body); err != nil {
		return "", fmt.Errorf("%w: %v", errProtocol, err)
	}
	return msg.Password, nil
}

// pgRefusal is a login the broker does not take on. It never holds a value.
// Kind is refuse, or upstream for the checks 6 and 7 of the caller. Secret is
// set when the event names a secret. OtherNS is set for check 1, which also
// logs a violation.
type pgRefusal struct {
	Code, Message string
	Kind          eventlog.Kind
	Secret        string
	OtherNS       string
}

// checkLogin runs the checks 1 to 5 of the login, in order, without a
// connection to the upstream. On success it returns the entry and a copy of
// the secret's value; the caller wipes it.
func checkLogin(ns string, pg config.Postgres, sec transform.Secrets, user, db, password string) (config.Database, []byte, *pgRefusal) {
	refuse := func(code, format string, args ...any) *pgRefusal {
		return &pgRefusal{Code: code, Message: fmt.Sprintf(format, args...), Kind: eventlog.KindRefuse}
	}
	for _, ref := range placeholder.Find(password) {
		if ref.Namespace != ns {
			r := refuse("28000", "placeholder of another namespace")
			r.OtherNS = ref.Namespace
			return config.Database{}, nil, r
		}
	}
	idx := slices.IndexFunc(pg.Databases, func(d config.Database) bool { return d.Alias == db })
	if idx < 0 {
		return config.Database{}, nil, refuse("3D000", "database %q is not configured in namespace %s", db, ns)
	}
	entry := pg.Databases[idx]
	if user != entry.User {
		return config.Database{}, nil, refuse("28000", "user %q may not use database %q", user, entry.Alias)
	}
	if password != placeholder.Format(ns, entry.Password) {
		return config.Database{}, nil, refuse("28P01", "password is not the placeholder of database %q", entry.Alias)
	}
	value, state := sec.Lookup(entry.Password)
	if state != secrets.StateOK {
		r := refuse("57P03", "secret not loaded: %s (%s)", entry.Password, state)
		r.Secret = entry.Password
		return config.Database{}, nil, r
	}
	return entry, value, nil
}

// writeFatal sends one ErrorResponse with severity FATAL.
func writeFatal(w io.Writer, code, msg string) error {
	buf, err := (&pgproto3.ErrorResponse{Severity: "FATAL", SeverityUnlocalized: "FATAL", Code: code, Message: msg}).Encode(nil)
	if err != nil {
		return err
	}
	_, err = w.Write(buf)
	return err
}
