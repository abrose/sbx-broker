// Package cloexec gives listeners whose accepted connections never leak into
// a child process.
//
// On darwin, Go's accept sets close-on-exec in a second step and does not
// hold syscall.ForkLock while it does (see internal/poll/sys_cloexec.go). A
// fetch command forked in between inherits the accepted connection, which
// then stays open until that child exits: an admin client waits for EOF up
// to the fetch timeout, an app connection outlives its listener (and keeps
// its port), and the fetch tool holds a socket of a sandboxed app. This
// listener accepts and sets close-on-exec under ForkLock, so no fork falls
// between the two.
package cloexec

import (
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
)

type filer interface {
	File() (*os.File, error)
}

type listener struct {
	net.Listener
	file   *os.File // a dup of the listening socket, on the poller
	raw    syscall.RawConn
	once   sync.Once
	closed atomic.Bool
}

// Listener wraps ln, a *net.TCPListener or a *net.UnixListener. Addr is
// that of ln; Close closes ln too.
func Listener(ln net.Listener) (net.Listener, error) {
	fl, ok := ln.(filer)
	if !ok {
		return nil, errors.New("cloexec: the listener has no file")
	}
	// A listener's own RawConn cannot wait for reads, but a file can. The
	// dup is close-on-exec and non-blocking like ln, so it is on the poller.
	f, err := fl.File()
	if err != nil {
		return nil, err
	}
	raw, err := f.SyscallConn()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &listener{Listener: ln, file: f, raw: raw}, nil
}

// Accept waits for a connection on the poller, then accepts it with
// close-on-exec set before any fork can run.
func (l *listener) Accept() (net.Conn, error) {
	nfd := -1
	var aerr error
	err := l.raw.Read(func(fd uintptr) bool {
		for {
			syscall.ForkLock.RLock()
			nfd, _, aerr = syscall.Accept(int(fd))
			if aerr == nil {
				syscall.CloseOnExec(nfd)
			}
			syscall.ForkLock.RUnlock()
			// As net does: retry an interrupted call and a connection
			// that was reset while it waited in the queue.
			if !errors.Is(aerr, syscall.EINTR) && !errors.Is(aerr, syscall.ECONNABORTED) {
				break
			}
		}
		return !errors.Is(aerr, syscall.EAGAIN) // EAGAIN: wait for the poller
	})
	if err != nil && l.closed.Load() {
		err = net.ErrClosed // the file says "use of closed file"
	}
	if err == nil && aerr != nil {
		err = os.NewSyscallError("accept", aerr)
	}
	if err != nil {
		return nil, &net.OpError{Op: "accept", Net: l.Addr().Network(), Addr: l.Addr(), Err: err}
	}
	f := os.NewFile(uintptr(nfd), "accepted")
	defer f.Close()        // net.FileConn holds its own (close-on-exec) dup
	return net.FileConn(f) // a *net.TCPConn or *net.UnixConn
}

// Close closes the dup (which ends a waiting Accept) and ln (which removes
// a Unix socket file).
func (l *listener) Close() error {
	var err error
	l.once.Do(func() {
		l.closed.Store(true)
		err = errors.Join(l.file.Close(), l.Listener.Close())
	})
	return err
}
