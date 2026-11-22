// Package admin is the broker's admin socket: a Unix socket in the state
// dir that the CLI (and later the TUI) uses. A client sends one JSON request
// line; the broker answers with JSON response lines and closes the
// connection after the last one. A "log" request with Follow stays open
// until the client or the broker closes it.
package admin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/abrose/sbx-broker/internal/cloexec"
	"github.com/abrose/sbx-broker/internal/eventlog"
	"github.com/abrose/sbx-broker/internal/secrets"
)

const (
	// SocketName is the file name of the socket in the state dir.
	SocketName = "admin.sock"
	socketMode = 0o600
	logBacklog = 100 // the events a "log" request gets first
	maxRequest = 64 << 10
	reqTimeout = 10 * time.Second // to send the request line
	probeWait  = time.Second      // to tell a live socket from a stale one
	acceptWait = 100 * time.Millisecond
)

var (
	// ErrRunning is the error of Listen when another broker answers on the socket.
	ErrRunning = errors.New("a broker already runs")
	// ErrNoBroker is the error of Dial when no broker answers on the socket.
	ErrNoBroker = errors.New("no broker runs")
)

// Request is one request line. Ops: "status", "refresh", "reload", "log".
type Request struct {
	Op        string `json:"op"`
	Namespace string `json:"ns,omitempty"`
	Secret    string `json:"secret,omitempty"`
	Follow    bool   `json:"follow,omitempty"`
}

// NamespaceStatus describes one namespace. It holds no value.
type NamespaceStatus struct {
	Name      string           `json:"name"`
	Listeners []string         `json:"listeners"`
	Secrets   []secrets.Status `json:"secrets"`
	Error     string           `json:"error,omitempty"`
}

// Response is one response line.
type Response struct {
	OK         bool              `json:"ok"`
	Error      string            `json:"error,omitempty"`
	Namespaces []NamespaceStatus `json:"namespaces,omitempty"`
	Event      *eventlog.Event   `json:"event,omitempty"`
}

// Backend is what the socket asks of the broker.
type Backend interface {
	// Status returns every namespace, or only ns when it is not empty.
	Status(ns string) ([]NamespaceStatus, error)
	// Refresh fetches one secret of ns again, or every secret when secret
	// is empty, and returns the status of ns after the fetch.
	Refresh(ctx context.Context, ns, secret string) ([]NamespaceStatus, error)
	// Reload scans the config dir now.
	Reload(ctx context.Context) error
}

// SocketPath returns the path of the socket in stateDir.
func SocketPath(stateDir string) string { return filepath.Join(stateDir, SocketName) }

// Listen listens on the socket in stateDir with mode 0600. A socket file
// that accepts a connection gives ErrRunning; one that refuses is stale and
// removed first.
func Listen(stateDir string) (net.Listener, error) {
	path := SocketPath(stateDir)
	fi, err := os.Lstat(path)
	switch {
	case err == nil:
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if c, err := net.DialTimeout("unix", path, probeWait); err == nil {
			_ = c.Close()
			return nil, ErrRunning
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	raw, err := net.Listen("unix", path)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, ErrRunning // another broker won the race
		}
		return nil, err
	}
	// A fetch command must not inherit an admin connection: the client
	// would wait for EOF until that command ends.
	ln, err := cloexec.Listener(raw)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	// The mode of a new socket follows the umask; set it outright.
	if err := os.Chmod(path, socketMode); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

// Server answers the requests on one listener.
type Server struct {
	ln     net.Listener
	b      Backend
	log    *eventlog.Log
	ctx    context.Context // cancelled by Close
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
}

// Serve answers on ln in the background until Close.
func Serve(ln net.Listener, b Backend, log *eventlog.Log) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{ln: ln, b: b, log: log, ctx: ctx, cancel: cancel, conns: map[net.Conn]struct{}{}}
	s.wg.Add(1)
	go s.accept()
	return s
}

// Close closes the listener (which removes the socket file) and every
// connection, and waits for the open requests to end.
func (s *Server) Close() error {
	var err error
	s.once.Do(func() {
		s.cancel()
		err = s.ln.Close()
		s.mu.Lock()
		s.closed = true
		for c := range s.conns {
			_ = c.Close()
		}
		s.mu.Unlock()
		s.wg.Wait()
	})
	return err
}

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if s.ctx.Err() != nil {
				return
			}
			time.Sleep(acceptWait) // such as too many open files
			continue
		}
		if !s.track(conn) {
			_ = conn.Close()
			return
		}
		s.wg.Add(1)
		go s.handle(conn)
	}
}

func (s *Server) track(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conns[c] = struct{}{}
	return true
}

func (s *Server) untrack(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	_ = c.Close()
}

func (s *Server) handle(conn net.Conn) {
	defer s.wg.Done()
	defer s.untrack(conn)
	_ = conn.SetReadDeadline(time.Now().Add(reqTimeout))
	r := bufio.NewReaderSize(conn, maxRequest)
	line, err := r.ReadSlice('\n')
	if err != nil && !(errors.Is(err, io.EOF) && len(line) > 0) {
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	enc := json.NewEncoder(conn)
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		_ = enc.Encode(Response{Error: "bad request"})
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	// The client sends nothing more; when it closes its end, a follow ends.
	go func() {
		_, _ = io.Copy(io.Discard, r)
		cancel()
	}()
	s.do(ctx, req, enc)
}

func (s *Server) do(ctx context.Context, req Request, enc *json.Encoder) {
	reply := func(nss []NamespaceStatus, err error) {
		if err != nil {
			_ = enc.Encode(Response{Error: err.Error()})
			return
		}
		_ = enc.Encode(Response{OK: true, Namespaces: nss})
	}
	switch req.Op {
	case "status":
		reply(s.b.Status(req.Namespace))
	case "refresh":
		if req.Namespace == "" {
			reply(nil, errors.New("refresh needs a namespace"))
			return
		}
		reply(s.b.Refresh(ctx, req.Namespace, req.Secret))
	case "reload":
		reply(nil, s.b.Reload(ctx))
	case "log":
		s.logOp(ctx, req, enc)
	default:
		reply(nil, fmt.Errorf("unknown op %s", req.Op))
	}
}

// logOp sends the last 100 events (of req.Namespace, when set), then with
// Follow each new one.
func (s *Server) logOp(ctx context.Context, req Request, enc *json.Encoder) {
	match := func(e eventlog.Event) bool { return req.Namespace == "" || e.Namespace == req.Namespace }
	var events <-chan eventlog.Event
	if req.Follow {
		// Subscribe before Recent, so no event falls between the two.
		var stop func()
		events, stop = s.log.Subscribe()
		defer stop()
	}
	var recent []eventlog.Event
	for _, e := range s.log.Recent(1 << 30) {
		if match(e) {
			recent = append(recent, e)
		}
	}
	if len(recent) > logBacklog {
		recent = recent[len(recent)-logBacklog:]
	}
	sent := map[string]bool{} // the recent events, to skip them once more in events
	for _, e := range recent {
		if req.Follow {
			sent[eventKey(e)] = true
		}
		if err := enc.Encode(Response{OK: true, Event: &e}); err != nil {
			return
		}
	}
	if !req.Follow {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-events:
			if !ok {
				return
			}
			if !match(e) {
				continue
			}
			// The events added between Subscribe and Recent come first and
			// were sent already.
			if sent != nil {
				if sent[eventKey(e)] {
					continue
				}
				sent = nil
			}
			if err := enc.Encode(Response{OK: true, Event: &e}); err != nil {
				return
			}
		}
	}
}

func eventKey(e eventlog.Event) string {
	b, _ := json.Marshal(e) // an Event always encodes
	return string(b)
}

// Client is one connection to the admin socket, for one request.
type Client struct{ conn net.Conn }

// Dial connects to the socket in stateDir. It returns ErrNoBroker when no
// socket file exists or none answers on it.
func Dial(stateDir string) (*Client, error) {
	c, err := net.Dial("unix", SocketPath(stateDir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			return nil, ErrNoBroker
		}
		return nil, err
	}
	return &Client{conn: c}, nil
}

// Do sends req and calls each for every response, until the broker closes
// the connection or each returns an error. A response with ok false ends Do
// with its error. Do closes the connection.
func (c *Client) Do(req Request, each func(Response) error) error {
	defer c.conn.Close()
	line, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if _, err := c.conn.Write(append(line, '\n')); err != nil {
		return err
	}
	dec := json.NewDecoder(c.conn)
	for {
		var resp Response
		if err := dec.Decode(&resp); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if !resp.OK {
			if resp.Error == "" {
				return errors.New("the broker refused the request")
			}
			return errors.New(resp.Error)
		}
		if err := each(resp); err != nil {
			return err
		}
	}
}

// Close closes the connection without a request.
func (c *Client) Close() error { return c.conn.Close() }
