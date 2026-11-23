// Package server is `sbx-broker serve`: it loads the namespace files, fetches
// the secrets, runs the listeners, watches the files, and answers the admin
// socket.
package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
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
	// A poll acts on a change that two polls in a row saw: with 1 s, a
	// change takes effect within 2 s.
	defaultPollInterval = time.Second
	rebindWait          = time.Second // see listen
	rebindStep          = 20 * time.Millisecond
	logName             = "broker.log"
	logRing             = 10000
	logMaxBytes         = 10 << 20
	stateDirMode        = 0o700
	redacted            = "[redacted]"
	postgresInfo        = "postgres listeners come in a later version"
)

// Options configures Run. A zero FetchTimeout means 60 s, a zero
// PollInterval 1 s.
type Options struct {
	ConfigDir, StateDir        string
	FetchTimeout, PollInterval time.Duration

	// beforeLogClose, if set, runs after the shutdown and before the log
	// closes: the last point a late event can come. Tests only.
	beforeLogClose func(*eventlog.Log)
}

// Run serves until ctx is done, then stops the file watch, closes every
// listener, the admin socket and the log, in that order, and releases the
// single-instance lock last. It returns nil after a clean stop.
func Run(ctx context.Context, opt Options) (err error) {
	if opt.FetchTimeout <= 0 {
		opt.FetchTimeout = defaultFetchTimeout
	}
	if opt.PollInterval <= 0 {
		opt.PollInterval = defaultPollInterval
	}
	if err := prepareStateDir(opt.StateDir); err != nil {
		return err
	}
	lock, err := admin.TakeLock(opt.StateDir)
	if err != nil {
		return err
	}
	// Last: a second broker must not start while this one still writes
	// the log or holds values.
	defer func() { err = errors.Join(err, lock.Release()) }()
	ln, err := lock.Listen()
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
	if err := b.scan(scanFirst); err != nil {
		_ = ln.Close()
		b.fetchStop()
		return errors.Join(fmt.Errorf("config dir: %w", err), log.Close())
	}
	srv := admin.Serve(ln, b, log)
	b.watch(ctx)
	b.shutdown(srv)
	if opt.beforeLogClose != nil {
		opt.beforeLogClose(log)
	}
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
	ports     map[int]bool       // the ports of the listeners
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
	stamps  map[string]stamp // by file name: the stamp last acted on
	seen    map[string]stamp // by file name: the stamps of the last scan
	refused map[string]bool  // by file name: did not load or clashed on a port
	dirErr  string           // the last config dir error logged

	mu  sync.Mutex
	nss map[string]*nsState // by namespace name
}

func newBroker(opt Options, log *eventlog.Log) *broker {
	ctx, cancel := context.WithCancel(context.Background())
	return &broker{
		opt: opt, log: log, red: &redactor{ns: map[string]*redactNS{}},
		fetchCtx: ctx, fetchStop: cancel, reloads: make(chan chan struct{}),
		stamps: map[string]stamp{}, seen: map[string]stamp{}, refused: map[string]bool{},
		nss: map[string]*nsState{},
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
			_ = b.scan(scanPoll) // a dir error is logged
		case done := <-b.reloads:
			_ = b.scan(scanReload)
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
	running := maps.Clone(b.nss)
	b.mu.Unlock()
	for name, st := range running {
		// As in stop: the redactor keeps copies until the log closed (Run
		// wipes them then), so a late event is still redacted.
		b.red.retire(name, st.store.Values())
		st.store.Wipe()
	}
}

func closeAll(hs []*proxy.HTTP) {
	var wg sync.WaitGroup
	for _, h := range hs {
		wg.Go(func() { _ = h.Close() }) // the error is an event of the listener
	}
	wg.Wait()
}

type scanMode int

const (
	// scanFirst is the scan at start: it acts at once, and returns a config
	// dir error without logging it.
	scanFirst scanMode = iota
	// scanPoll acts on a change (a new, changed or removed file) only when
	// the previous scan saw the same state: an editor that truncates the
	// file, then writes it, must not stop the namespace in between.
	scanPoll
	// scanReload is the reload op: it acts at once, and also loads again
	// every namespace with a listener that did not start and every file
	// that was refused (a port clash, or an error that may be gone).
	scanReload
)

// scan loads the files that are new or changed since they were last acted
// on and stops the namespaces whose file is gone, as mode says.
func (b *broker) scan(mode scanMode) error {
	dir := b.opt.ConfigDir
	stamps, err := readStamps(dir)
	var ok map[string]*config.Namespace
	var bad map[string]error
	if err == nil {
		ok, bad, err = config.LoadDir(dir)
	}
	if err != nil {
		if mode != scanFirst && err.Error() != b.dirErr {
			b.dirErr = err.Error()
			b.log.Add(eventlog.Event{Kind: eventlog.KindError, Message: "config dir: " + err.Error()})
		}
		return err
	}
	b.dirErr = ""
	prev := b.seen
	b.seen = stamps
	stable := func(file string) bool {
		if mode != scanPoll {
			return true
		}
		s, there := stamps[file]
		p, was := prev[file]
		return there == was && s == p
	}
	for _, file := range slices.Sorted(mapKeys(b.stamps)) {
		if _, there := stamps[file]; !there && stable(file) {
			delete(b.stamps, file)
			delete(b.refused, file)
			b.remove(strings.TrimSuffix(file, ".yaml"))
		}
	}
	for _, file := range slices.Sorted(mapKeys(stamps)) {
		s := stamps[file]
		name := strings.TrimSuffix(file, ".yaml")
		old, acted := b.stamps[file]
		if changed := !acted || old != s; changed && !stable(file) ||
			!changed && (mode != scanReload || !b.needsRetry(file, name)) {
			continue
		}
		if lerr, isBad := bad[file]; isBad {
			// LoadDir refuses a file that claims a port of an earlier file.
			b.stamps[file] = s
			b.refused[file] = true
			b.logBadFile(name, file, lerr)
			continue
		}
		cfg := ok[name]
		if cfg == nil {
			continue // it came or went between the stamp and the load: the next scan sees it
		}
		b.stamps[file] = s
		if perr := b.portClash(cfg); perr != nil {
			b.refused[file] = true
			b.logBadFile(name, file, perr)
			continue
		}
		delete(b.refused, file)
		b.replace(cfg)
	}
	return nil
}

// needsRetry reports a file that was refused (it did not load, or it
// clashed on a port), or a namespace with a listener that did not start.
func (b *broker) needsRetry(file, name string) bool {
	if b.refused[file] {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.nss[name]
	return st != nil && len(st.errs) > 0
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
	var held map[int]bool
	if old != nil {
		b.stop(old) // first: the new listeners may reuse its ports
		verb, held = "reloaded", old.ports
	}
	st := b.start(cfg, verb, held)
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
// in the redactor, by secret name, until the namespace is removed: the next
// state of the namespace scrubs them from its answers, also when a secret
// is gone from the file.
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
func (b *broker) start(cfg *config.Namespace, verb string, held map[int]bool) *nsState {
	// A refresh that fails or changes a value retires the old one first.
	retire := func(name string, v []byte) { b.red.retireOne(cfg.Name, name, v) }
	st := &nsState{
		cfg:     cfg,
		store:   secrets.NewStore(cfg.Name, cfg.Secrets, b.opt.FetchTimeout, b.log, retire),
		ports:   map[int]bool{},
		fetched: make(chan struct{}),
	}
	st.ctx, st.cancel = context.WithCancel(b.fetchCtx)
	b.red.set(cfg.Name, st.store)
	b.log.Add(eventlog.Event{Kind: eventlog.KindReload, Namespace: cfg.Name, Message: verb})
	sec := nsSecrets{Store: st.store, ns: cfg.Name, red: b.red}
	for _, r := range cfg.HTTP {
		h, err := b.listen(cfg.Name, r, sec, held[r.Listen])
		if err != nil {
			st.errs = append(st.errs, err.Error())
			b.log.Add(eventlog.Event{Kind: eventlog.KindError, Namespace: cfg.Name, Route: r.Name, Message: err.Error()})
			continue
		}
		st.listeners = append(st.listeners, h)
		st.ports[r.Listen] = true
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

// listen starts the listener of route r. With held set, the old state of
// the namespace held the port: the old socket is closed, but a fetch
// command forked a moment before holds a copy until its exec, so a bind
// that finds the port in use is tried again for up to rebindWait.
func (b *broker) listen(ns string, r config.Route, sec transform.Secrets, held bool) (*proxy.HTTP, error) {
	trs := make([]transform.Transform, 0, len(r.Transforms))
	for _, spec := range r.Transforms {
		tr, err := transform.New(spec)
		if err != nil {
			return nil, fmt.Errorf("route %s: %w", r.Name, err)
		}
		trs = append(trs, tr)
	}
	h := proxy.NewHTTP(ns, r, sec, trs, b.log)
	deadline := time.Now().Add(rebindWait)
	for {
		err := h.Start()
		if err == nil {
			return h, nil
		}
		if !held || !errors.Is(err, syscall.EADDRINUSE) || time.Now().After(deadline) {
			return nil, err // it names the route
		}
		time.Sleep(rebindStep)
	}
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
	// The store retires each value the fetch drops or replaces (see start).
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

// nsSecrets is what the listeners of a namespace see: the current store for
// the requests, plus the values the namespace retired for the answer scrub.
type nsSecrets struct {
	*secrets.Store
	ns  string
	red *redactor
}

var _ transform.Secrets = nsSecrets{}

// Retired implements transform.Secrets.
func (s nsSecrets) Retired() map[string][][]byte { return s.red.retiredCopies(s.ns) }

// redactor keeps the values the broker holds and held: the ok values of
// every running namespace, read from the stores at call time, and the values
// that left the ok state (a failed or changing refresh, a reload, a secret
// removed from the file), by secret name, until the namespace is removed or
// the broker stops. It replaces each in a log text, and gives the retired
// ones to the answer scrub (finding C1). redact runs under the log's mutex,
// so the redactor never calls the log.
type redactor struct {
	mu sync.RWMutex
	ns map[string]*redactNS
}

type redactNS struct {
	store   *secrets.Store
	retired map[string][][]byte // by secret name; copies the redactor owns
}

// set makes s the current store of ns. Retired values stay.
func (r *redactor) set(ns string, s *secrets.Store) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.ns[ns]
	if e == nil {
		e = &redactNS{retired: map[string][][]byte{}}
		r.ns[ns] = e
	}
	e.store = s
}

// retire keeps the values (copies it owns) of ns, by secret name, until
// drop.
func (r *redactor) retire(ns string, vals map[string][]byte) {
	for name, v := range vals {
		r.retireOne(ns, name, v)
	}
}

// retireOne keeps v (a copy it owns) as a retired value of secret name in
// ns. A value it has already, or one of a namespace it does not know, is
// wiped.
func (r *redactor) retireOne(ns, name string, v []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.ns[ns]
	if e == nil || slices.ContainsFunc(e.retired[name], func(old []byte) bool { return bytes.Equal(old, v) }) {
		clear(v)
		return
	}
	e.retired[name] = append(e.retired[name], v)
}

// retiredCopies returns copies of the retired values of ns, by secret name.
func (r *redactor) retiredCopies(ns string) map[string][][]byte {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := map[string][][]byte{}
	if e := r.ns[ns]; e != nil {
		for name, vs := range e.retired {
			for _, v := range vs {
				out[name] = append(out[name], append([]byte(nil), v...))
			}
		}
	}
	return out
}

// drop forgets ns and wipes its retired values.
func (r *redactor) drop(ns string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.ns[ns]; e != nil {
		for _, vs := range e.retired {
			for _, v := range vs {
				clear(v)
			}
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
		for _, vs := range e.retired {
			all = append(all, vs...)
		}
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
