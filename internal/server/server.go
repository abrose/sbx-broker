// Package server is `sbx-broker serve`: it loads the namespace files, fetches
// the secrets, runs the listeners, watches the files, and answers the admin
// socket.
package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/abrose/sbx-broker/internal/admin"
	"github.com/abrose/sbx-broker/internal/config"
	"github.com/abrose/sbx-broker/internal/eventlog"
	"github.com/abrose/sbx-broker/internal/placeholder"
	"github.com/abrose/sbx-broker/internal/proxy"
	"github.com/abrose/sbx-broker/internal/secrets"
	"github.com/abrose/sbx-broker/internal/transform"
)

const (
	defaultFetchTimeout = 60 * time.Second
	defaultPollInterval = 2 * time.Second
	logName             = "broker.log"
	logRing             = 10000
	logMaxBytes         = 10 << 20
	stateDirMode        = 0o700
	redacted            = "[redacted]"
	postgresInfo        = "postgres listeners come in a later version"
)

// Options configures Run. A zero FetchTimeout means 60 s, a zero
// PollInterval 2 s.
type Options struct {
	ConfigDir, StateDir        string
	FetchTimeout, PollInterval time.Duration
}

// Run serves until ctx is done, then stops the file watch, closes every
// listener, the admin socket and the log, in that order. It returns nil
// after a clean stop.
func Run(ctx context.Context, opt Options) error {
	if opt.FetchTimeout <= 0 {
		opt.FetchTimeout = defaultFetchTimeout
	}
	if opt.PollInterval <= 0 {
		opt.PollInterval = defaultPollInterval
	}
	if err := prepareStateDir(opt.StateDir); err != nil {
		return err
	}
	ln, err := admin.Listen(opt.StateDir)
	if err != nil {
		return err
	}
	log, err := eventlog.Open(filepath.Join(opt.StateDir, logName), logRing, logMaxBytes)
	if err != nil {
		_ = ln.Close()
		return err
	}
	b := newBroker(opt, log)
	log.SetRedactor(b.red.redact)
	if err := b.scan(true); err != nil {
		_ = ln.Close()
		b.fetchStop()
		return errors.Join(fmt.Errorf("config dir: %w", err), log.Close())
	}
	srv := admin.Serve(ln, b, log)
	b.watch(ctx)
	b.shutdown(srv)
	err = log.Close()
	b.red.wipeAll()
	return err
}

// prepareStateDir makes the state dir with mode 0700, or tightens the mode
// of one that exists. It refuses a symlink and a dir of another user.
func prepareStateDir(dir string) error {
	if err := os.MkdirAll(dir, stateDirMode); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("state dir %s is a symlink", dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("state dir %s is not a directory", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("state dir %s is not owned by the current user", dir)
	}
	if fi.Mode().Perm() != stateDirMode {
		return os.Chmod(dir, stateDirMode)
	}
	return nil
}

// stamp tells whether a namespace file changed since the last scan.
type stamp struct {
	mod  time.Time
	size int64
	mode os.FileMode
}

// nsState is one running namespace.
type nsState struct {
	cfg       *config.Namespace
	store     *secrets.Store
	listeners []*proxy.HTTP
	errs      []string           // listeners that did not start
	ctx       context.Context    // of its fetches; cancelled when it stops
	cancel    context.CancelFunc //
	fetched   chan struct{}      // closed when the first FetchAll returned
}

func (st *nsState) defines(secret string) bool {
	for _, s := range st.cfg.Secrets {
		if s.Name == secret {
			return true
		}
	}
	return false
}

func (st *nsState) status() admin.NamespaceStatus {
	addrs := make([]string, 0, len(st.listeners))
	for _, h := range st.listeners {
		addrs = append(addrs, h.Addr())
	}
	return admin.NamespaceStatus{Name: st.cfg.Name, Listeners: addrs, Secrets: st.store.Statuses(), Error: strings.Join(st.errs, "; ")}
}

type broker struct {
	opt       Options
	log       *eventlog.Log
	red       *redactor
	fetchCtx  context.Context // the parent of every namespace's fetches
	fetchStop context.CancelFunc
	fetches   sync.WaitGroup
	reloads   chan chan struct{}

	// Only the watch goroutine uses these.
	stamps map[string]stamp // by file name
	dirErr string           // the last config dir error logged

	mu  sync.Mutex
	nss map[string]*nsState // by namespace name
}

func newBroker(opt Options, log *eventlog.Log) *broker {
	ctx, cancel := context.WithCancel(context.Background())
	return &broker{
		opt: opt, log: log, red: &redactor{ns: map[string]*redactNS{}},
		fetchCtx: ctx, fetchStop: cancel, reloads: make(chan chan struct{}),
		stamps: map[string]stamp{}, nss: map[string]*nsState{},
	}
}

// watch scans the config dir every PollInterval, and on a reload request,
// until ctx is done.
func (b *broker) watch(ctx context.Context) {
	t := time.NewTicker(b.opt.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = b.scan(false) // a dir error is logged
		case done := <-b.reloads:
			_ = b.scan(false)
			close(done)
		}
	}
}

// shutdown runs after the watch ended: it kills the fetches, closes every
// listener (they may still write events), the admin socket, and waits for
// the fetches, so nothing writes to the log after it returns.
func (b *broker) shutdown(srv *admin.Server) {
	b.fetchStop()
	b.mu.Lock()
	var all []*proxy.HTTP
	for _, st := range b.nss {
		all = append(all, st.listeners...)
	}
	b.mu.Unlock()
	closeAll(all)
	_ = srv.Close() // waits for the admin requests, a refresh among them
	b.fetches.Wait()
	b.mu.Lock()
	for _, st := range b.nss {
		st.store.Wipe()
	}
	b.mu.Unlock()
}

func closeAll(hs []*proxy.HTTP) {
	var wg sync.WaitGroup
	for _, h := range hs {
		wg.Go(func() { _ = h.Close() }) // the error is an event of the listener
	}
	wg.Wait()
}

// scan loads the files that are new or changed since the last scan and
// stops the namespaces whose file is gone. With first set, a config dir
// error is returned and not logged.
func (b *broker) scan(first bool) error {
	dir := b.opt.ConfigDir
	stamps, err := readStamps(dir)
	var ok map[string]*config.Namespace
	var bad map[string]error
	if err == nil {
		ok, bad, err = config.LoadDir(dir)
	}
	if err != nil {
		if !first && err.Error() != b.dirErr {
			b.dirErr = err.Error()
			b.log.Add(eventlog.Event{Kind: eventlog.KindError, Message: "config dir: " + err.Error()})
		}
		return err
	}
	b.dirErr = ""
	for _, file := range slices.Sorted(mapKeys(b.stamps)) {
		if _, there := stamps[file]; !there {
			delete(b.stamps, file)
			b.remove(strings.TrimSuffix(file, ".yaml"))
		}
	}
	for _, file := range slices.Sorted(mapKeys(stamps)) {
		s := stamps[file]
		if old, seen := b.stamps[file]; seen && old == s {
			continue
		}
		name := strings.TrimSuffix(file, ".yaml")
		if lerr, isBad := bad[file]; isBad {
			b.stamps[file] = s
			b.logBadFile(name, file, lerr)
			continue
		}
		cfg := ok[name]
		if cfg == nil {
			continue // it came or went between the stamp and the load: the next scan sees it
		}
		b.stamps[file] = s
		if perr := b.portClash(cfg); perr != nil {
			b.logBadFile(name, file, perr)
			continue
		}
		b.replace(cfg)
	}
	return nil
}

func mapKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// readStamps returns the stamp of every *.yaml file in dir.
func readStamps(dir string) (map[string]stamp, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]stamp{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		fi, err := os.Lstat(filepath.Join(dir, e.Name()))
		if err != nil {
			continue // gone since ReadDir
		}
		out[e.Name()] = stamp{mod: fi.ModTime(), size: fi.Size(), mode: fi.Mode()}
	}
	return out, nil
}

// logBadFile logs a file that did not load. Its namespace, if it runs,
// keeps its old state.
func (b *broker) logBadFile(name, file string, err error) {
	e := eventlog.Event{Kind: eventlog.KindError, Message: file + ": " + err.Error()}
	if placeholder.ValidNamespace(name) {
		e.Namespace = name
	}
	b.log.Add(e)
}

// portClash reports a port of cfg that a running namespace of another file
// holds.
func (b *broker) portClash(cfg *config.Namespace) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for name, st := range b.nss {
		if name == cfg.Name {
			continue
		}
		for _, p := range cfg.Ports() {
			if slices.Contains(st.cfg.Ports(), p) {
				return fmt.Errorf("port %d is used by namespace %s", p, name)
			}
		}
	}
	return nil
}

// replace stops the old state of cfg's namespace, if any, then starts it.
func (b *broker) replace(cfg *config.Namespace) {
	b.mu.Lock()
	old := b.nss[cfg.Name]
	b.mu.Unlock()
	verb := "loaded"
	if old != nil {
		b.stop(old) // first: the new listeners may reuse its ports
		verb = "reloaded"
	}
	st := b.start(cfg, verb)
	b.mu.Lock()
	b.nss[cfg.Name] = st
	b.mu.Unlock()
}

// remove stops a namespace whose file is gone and forgets its values.
func (b *broker) remove(name string) {
	b.mu.Lock()
	st := b.nss[name]
	delete(b.nss, name)
	b.mu.Unlock()
	if st == nil {
		return // its file never loaded
	}
	b.stop(st)
	b.red.drop(name)
	b.log.Add(eventlog.Event{Kind: eventlog.KindReload, Namespace: name, Message: "stopped"})
}

// stop cancels the fetches of st and closes its listeners. Its values stay
// in the redactor until the namespace is removed.
func (b *broker) stop(st *nsState) {
	name := st.cfg.Name
	b.red.retire(name, st.store.Values()) // before the cancel, which may wipe them
	st.cancel()
	closeAll(st.listeners)
	<-st.fetched // the cancel kills the commands at once
	b.red.retire(name, st.store.Values())
	// The redactor keeps its own copies; the store's go now. A refresh
	// that still runs under the cancelled ctx wipes its value on record.
	st.store.Wipe()
}

// start opens the listeners of cfg, then fetches its secrets in the
// background. A listener that does not start is logged and shown in status;
// the others run.
func (b *broker) start(cfg *config.Namespace, verb string) *nsState {
	st := &nsState{
		cfg:     cfg,
		store:   secrets.NewStore(cfg.Name, cfg.Secrets, b.opt.FetchTimeout, b.log),
		fetched: make(chan struct{}),
	}
	st.ctx, st.cancel = context.WithCancel(b.fetchCtx)
	b.red.set(cfg.Name, st.store)
	b.log.Add(eventlog.Event{Kind: eventlog.KindReload, Namespace: cfg.Name, Message: verb})
	for _, r := range cfg.HTTP {
		h, err := b.listen(cfg.Name, r, st.store)
		if err != nil {
			st.errs = append(st.errs, err.Error())
			b.log.Add(eventlog.Event{Kind: eventlog.KindError, Namespace: cfg.Name, Route: r.Name, Message: err.Error()})
			continue
		}
		st.listeners = append(st.listeners, h)
	}
	if cfg.Postgres != nil {
		b.log.Add(eventlog.Event{Kind: eventlog.KindInfo, Namespace: cfg.Name, Message: postgresInfo})
	}
	b.fetches.Add(1)
	go func() {
		defer b.fetches.Done()
		defer close(st.fetched)
		st.store.FetchAll(st.ctx)
	}()
	return st
}

func (b *broker) listen(ns string, r config.Route, store *secrets.Store) (*proxy.HTTP, error) {
	trs := make([]transform.Transform, 0, len(r.Transforms))
	for _, spec := range r.Transforms {
		tr, err := transform.New(spec)
		if err != nil {
			return nil, fmt.Errorf("route %s: %w", r.Name, err)
		}
		trs = append(trs, tr)
	}
	h := proxy.NewHTTP(ns, r, store, trs, b.log)
	if err := h.Start(); err != nil {
		return nil, err // it names the route
	}
	return h, nil
}

// Status implements admin.Backend.
func (b *broker) Status(ns string) ([]admin.NamespaceStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ns != "" {
		st := b.nss[ns]
		if st == nil {
			return nil, fmt.Errorf("unknown namespace %s", ns)
		}
		return []admin.NamespaceStatus{st.status()}, nil
	}
	out := make([]admin.NamespaceStatus, 0, len(b.nss))
	for _, name := range slices.Sorted(mapKeys(b.nss)) {
		out = append(out, b.nss[name].status())
	}
	return out, nil
}

// Refresh implements admin.Backend. The fetch runs under the namespace's
// context, so a reload or the stop of the broker cancels it.
func (b *broker) Refresh(_ context.Context, ns, secret string) ([]admin.NamespaceStatus, error) {
	b.mu.Lock()
	st := b.nss[ns]
	b.mu.Unlock()
	if st == nil {
		return nil, fmt.Errorf("unknown namespace %s", ns)
	}
	if secret != "" && !st.defines(secret) {
		return nil, fmt.Errorf("unknown secret %s in namespace %s", secret, ns)
	}
	// The values the fetch replaces stay redacted.
	b.red.retire(ns, st.store.Values())
	if secret == "" {
		st.store.FetchAll(st.ctx)
	} else {
		_ = st.store.Fetch(st.ctx, secret) // the outcome is in the status
	}
	return b.Status(ns)
}

// Reload implements admin.Backend: the watch scans the config dir now.
func (b *broker) Reload(ctx context.Context) error {
	done := make(chan struct{})
	select {
	case b.reloads <- done:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// redactor replaces every value the broker holds in a log text: the ok
// values of every running namespace, read from the stores at call time,
// and the values a refresh or a reload replaced, until the namespace is
// removed. It runs under the log's mutex, so it never calls the log.
type redactor struct {
	mu sync.RWMutex
	ns map[string]*redactNS
}

type redactNS struct {
	store   *secrets.Store
	retired [][]byte
}

// set makes s the current store of ns. Retired values stay.
func (r *redactor) set(ns string, s *secrets.Store) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.ns[ns]
	if e == nil {
		e = &redactNS{}
		r.ns[ns] = e
	}
	e.store = s
}

// retire keeps the values (copies it owns) of ns until drop.
func (r *redactor) retire(ns string, vals map[string][]byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.ns[ns]
	for _, v := range vals {
		if e == nil || slices.ContainsFunc(e.retired, func(old []byte) bool { return bytes.Equal(old, v) }) {
			clear(v)
			continue
		}
		e.retired = append(e.retired, v)
	}
}

// drop forgets ns and wipes its retired values.
func (r *redactor) drop(ns string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.ns[ns]; e != nil {
		for _, v := range e.retired {
			clear(v)
		}
		delete(r.ns, ns)
	}
}

func (r *redactor) wipeAll() {
	r.mu.Lock()
	names := slices.Collect(mapKeys(r.ns))
	r.mu.Unlock()
	for _, n := range names {
		r.drop(n)
	}
}

// pathEscape is what url.URL.EscapedPath does to v inside a decoded path,
// on bytes: a string copy of a value could never be wiped. The escape is
// per byte, so a value has this form wherever it sits in a path.
// TestPathEscapeMatchesURL holds it to the url package.
func pathEscape(v []byte) []byte {
	const upperHex = "0123456789ABCDEF"
	out := make([]byte, 0, len(v)+8)
	for _, c := range v {
		if pathKeeps(c) {
			out = append(out, c)
			continue
		}
		out = append(out, '%', upperHex[c>>4], upperHex[c&0xf])
	}
	return out
}

// pathKeeps reports the bytes a path keeps unescaped (url's shouldEscape
// in path mode).
func pathKeeps(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	}
	return strings.IndexByte("-_.~$&+,/:;=@", c) >= 0
}

func (r *redactor) redact(s string) string {
	if s == "" {
		return s
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var fresh, all [][]byte
	for _, e := range r.ns {
		for _, v := range e.store.Values() {
			fresh = append(fresh, v)
		}
		all = append(all, e.retired...)
	}
	all = append(all, fresh...)
	// The proxy logs the escaped path (url.URL.EscapedPath of the decoded
	// path); a value in it has this form.
	var escaped [][]byte
	for _, v := range all {
		if e := pathEscape(v); !bytes.Equal(e, v) {
			escaped = append(escaped, e)
		}
	}
	all = append(all, escaped...)
	defer func() {
		for _, v := range escaped {
			clear(v)
		}
	}()
	// Longer values first, so a value inside another goes with it. Bytes,
	// not strings: a string copy of a value could never be wiped.
	sort.Slice(all, func(i, j int) bool { return len(all[i]) > len(all[j]) })
	text := []byte(s)
	changed := false
	for _, v := range all {
		if len(v) > 0 && bytes.Contains(text, v) {
			text = bytes.ReplaceAll(text, v, []byte(redacted))
			changed = true
		}
	}
	for _, v := range fresh {
		clear(v)
	}
	if !changed {
		return s
	}
	return string(text)
}
