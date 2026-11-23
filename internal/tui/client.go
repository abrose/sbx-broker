package tui

import (
	"context"

	"github.com/abrose/sbx-broker/internal/admin"
	"github.com/abrose/sbx-broker/internal/eventlog"
)

// Client is what the TUI asks of the broker.
type Client interface {
	// Status returns every namespace, or only ns when it is not empty.
	Status(ns string) ([]admin.NamespaceStatus, error)
	// Refresh fetches one secret of ns again, or all of them when secret is
	// empty, and returns the status of ns after the fetch.
	Refresh(ns, secret string) ([]admin.NamespaceStatus, error)
	// Reload makes the broker read the namespace files again now.
	Reload() error
	// Reveal returns the value of one secret; the broker logs the reveal.
	// The caller owns the slice and wipes it.
	Reveal(ns, secret string) ([]byte, error)
	// Follow calls each for the recent events and then for every new one.
	// It returns when ctx ends or the connection drops.
	Follow(ctx context.Context, each func(eventlog.Event)) error
}

// NewSocketClient returns a Client that dials the admin socket in stateDir
// once per call and waits w for an answer: a call to a broker that accepts
// and never answers returns admin.ErrNoAnswer.
func NewSocketClient(stateDir string, w admin.Waits) Client {
	return socketClient{stateDir: stateDir, w: w}
}

type socketClient struct {
	stateDir string
	w        admin.Waits
}

func (c socketClient) do(req admin.Request, each func(admin.Response) error) error {
	cl, err := admin.DialWaits(c.stateDir, c.w)
	if err != nil {
		return err
	}
	return cl.Do(req, each)
}

func (c socketClient) namespaces(req admin.Request) ([]admin.NamespaceStatus, error) {
	var out []admin.NamespaceStatus
	err := c.do(req, func(r admin.Response) error {
		out = append(out, r.Namespaces...)
		return nil
	})
	return out, err
}

func (c socketClient) Status(ns string) ([]admin.NamespaceStatus, error) {
	return c.namespaces(admin.Request{Op: "status", Namespace: ns})
}

func (c socketClient) Refresh(ns, secret string) ([]admin.NamespaceStatus, error) {
	return c.namespaces(admin.Request{Op: "refresh", Namespace: ns, Secret: secret})
}

func (c socketClient) Reload() error {
	return c.do(admin.Request{Op: "reload"}, func(admin.Response) error { return nil })
}

// Reveal returns the decoded Value of the answer as it is, without a copy,
// so the slice the caller wipes is the only one besides the client's read
// buffer.
func (c socketClient) Reveal(ns, secret string) ([]byte, error) {
	var value []byte
	err := c.do(admin.Request{Op: "reveal", Namespace: ns, Secret: secret}, func(r admin.Response) error {
		clear(value) // only one answer comes; wipe an earlier one all the same
		value = r.Value
		return nil
	})
	if err != nil {
		clear(value)
		return nil, err
	}
	return value, nil
}

func (c socketClient) Follow(ctx context.Context, each func(eventlog.Event)) error {
	cl, err := admin.DialWaits(c.stateDir, c.w)
	if err != nil {
		return err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = cl.Close() // ends Do
		case <-done:
		}
	}()
	err = cl.Do(admin.Request{Op: "log", Follow: true}, func(r admin.Response) error {
		if r.Event != nil {
			each(*r.Event)
		}
		return nil
	})
	if ctx.Err() != nil {
		return nil
	}
	return err
}
