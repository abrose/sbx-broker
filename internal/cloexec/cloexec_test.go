package cloexec

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// stalls accepts n connections on ln while other goroutines start child
// processes all the time. The server closes each connection at once; a
// client that does not see EOF within 500 ms counts as a stall: a child
// holds a copy of the server's end.
func stalls(t *testing.T, ln net.Listener, n int) int {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	slots := make(chan struct{}, 32) // at most 32 children at a time
	for range 4 {
		wg.Go(func() {
			for ctx.Err() == nil {
				slots <- struct{}{}
				// A child lives longer than the client waits for EOF.
				cmd := exec.Command("sleep", "0.7")
				if err := cmd.Start(); err != nil {
					<-slots
					continue
				}
				wg.Go(func() {
					_ = cmd.Wait()
					<-slots
				})
			}
		})
	}
	defer func() {
		cancel()
		wg.Wait()
	}()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	var stalled atomic.Int32
	var cw sync.WaitGroup
	for range n {
		c, err := dial(ln.Addr())
		if err != nil {
			t.Fatal(err)
		}
		cw.Go(func() {
			defer c.Close()
			_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			if _, err := io.ReadAll(c); err != nil {
				stalled.Add(1)
			}
		})
	}
	cw.Wait()
	return int(stalled.Load())
}

// dial retries a refused dial for a while: a full Unix socket backlog
// refuses on macOS instead of making the client wait.
func dial(addr net.Addr) (net.Conn, error) {
	deadline := time.Now().Add(2 * time.Second)
	for {
		c, err := net.Dial(addr.Network(), addr.String())
		if err == nil || !errors.Is(err, syscall.ECONNREFUSED) || time.Now().After(deadline) {
			return c, err
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTCPListenerLeaksNoConnection(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := Listener(raw)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if n := stalls(t, ln, 300); n > 0 {
		t.Fatalf("%d of 300 connections stayed open in a child", n)
	}
}

func TestUnixListenerLeaksNoConnection(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "sbxb") // a short socket path (ruling R5)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	raw, err := net.Listen("unix", filepath.Join(dir, "s.sock"))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := Listener(raw)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if n := stalls(t, ln, 300); n > 0 {
		t.Fatalf("%d of 300 connections stayed open in a child", n)
	}
	if _, ok := ln.Addr().(*net.UnixAddr); !ok {
		t.Fatalf("addr %T", ln.Addr())
	}
}

func TestAcceptAfterClose(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := Listener(raw)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := ln.Accept()
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	_ = ln.Close()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Accept did not end")
	}
}

func TestAcceptedConnWorks(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := Listener(raw)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(c, io.LimitReader(c, 5))
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(c)
	if err != nil || string(got) != "hello" {
		t.Fatalf("got %q %v", got, err)
	}
}
