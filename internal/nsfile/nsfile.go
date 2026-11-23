// Package nsfile edits one namespace file in place: it keeps the comments and
// the key order, and saves only after a check, atomically.
package nsfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"

	"github.com/abrose/sbx-broker/internal/config"
)

var (
	// ErrChanged: the file on disk differs from the bytes the edit started from.
	ErrChanged = errors.New("the namespace file changed on disk; the TUI read it again, make the change again")
	// ErrNotFound: a remove named a secret, route or database that is not there.
	ErrNotFound = errors.New("not found")
)

type Route struct {
	Name     string
	Listen   int
	Upstream string
}

type Database struct{ Alias, Upstream, DBName, User, Password, RequireAuth string }

// File is a namespace file held as a yaml node tree. Edits change the tree in
// memory; Save writes it.
type File struct {
	path string
	orig []byte // the bytes on disk the tree was read from
	// prefix holds the text of a file without content (comments only), which
	// the yaml node tree cannot keep. Bytes writes it before the mapping.
	prefix string
	doc    *yaml.Node // the document node, which holds the head comment; nil without content
	root   *yaml.Node // the top-level mapping
}

// Open reads path under the rules of config.LoadDir: no symlink, a regular
// file, owned by the user, not writable by group or others.
func Open(path string) (*File, error) {
	if err := config.CheckOwnerOnlyFile(path); err != nil {
		return nil, err
	}
	data, err := readNoFollow(path)
	if err != nil {
		return nil, err
	}
	f := &File{path: path, orig: data}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		f.prefix = string(data)
		if f.prefix != "" && !strings.HasSuffix(f.prefix, "\n") {
			f.prefix += "\n"
		}
		f.root = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		return f, nil
	}
	if doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: the top level is not a mapping", path)
	}
	f.doc, f.root = &doc, doc.Content[0]
	return f, nil
}

func readNoFollow(path string) ([]byte, error) {
	fh, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	return io.ReadAll(fh)
}

// Bytes encodes the file with a two-space indent.
func (f *File) Bytes() ([]byte, error) {
	if len(f.root.Content) == 0 {
		// Nothing to encode: write the comments as text, so an emptied file
		// keeps its header.
		out := f.prefix
		if f.doc != nil {
			for _, c := range []string{f.doc.HeadComment, f.doc.FootComment} {
				if c != "" {
					out += c + "\n"
				}
			}
		}
		return []byte(out), nil
	}
	var buf bytes.Buffer
	if f.prefix != "" {
		// the kept comments, then a blank line before the first key
		buf.WriteString(f.prefix + "\n")
	}
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	top := f.doc
	if top == nil {
		top = f.root
	}
	if err := enc.Encode(top); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Save writes the edited file: it refuses when the file on disk is not the one
// Open read, runs check on the new bytes, writes .<ns>.yaml.tmp (0600,
// exclusive, no symlink), syncs it and renames it over the file. The reload is
// the caller's.
func (f *File) Save(check func(data []byte) error) error {
	disk, err := readNoFollow(f.path)
	if err != nil {
		return err
	}
	if !bytes.Equal(disk, f.orig) {
		return ErrChanged
	}
	data, err := f.Bytes()
	if err != nil {
		return err
	}
	if check != nil {
		if err := check(data); err != nil {
			return err
		}
	}
	tmp := filepath.Join(filepath.Dir(f.path), "."+filepath.Base(f.path)+".tmp")
	// A crash can leave a stale tmp. Remove it (a symlink itself, never its
	// target) so it cannot block every later save.
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := writeTmp(tmp, data); err != nil {
		return err
	}
	if err := os.Rename(tmp, f.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	f.orig = data
	return syncDir(filepath.Dir(f.path))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

func writeTmp(tmp string, data []byte) (err error) {
	fh, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = fh.Write(data); err == nil {
		err = fh.Chmod(0o600)
	}
	if err == nil {
		err = fh.Sync()
	}
	if cerr := fh.Close(); err == nil {
		err = cerr
	}
	return err
}

// SetSecret adds the secret, or replaces the command of the existing one.
func (f *File) SetSecret(name string, command []string) error {
	secrets, err := f.section("secrets", yaml.MappingNode)
	if err != nil {
		return err
	}
	entry, err := child(secrets, name, yaml.MappingNode)
	if err != nil {
		return fmt.Errorf("secret %q: %w", name, err)
	}
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
	for _, a := range command {
		seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: a, Style: yaml.DoubleQuotedStyle})
	}
	if old := get(entry, "command"); old != nil {
		seq.HeadComment, seq.LineComment, seq.FootComment = old.HeadComment, old.LineComment, old.FootComment
	}
	setNode(entry, "command", seq)
	return nil
}

// Kind is a kind of entry of a namespace file.
type Kind int

const (
	KindSecret   Kind = iota // a key of secrets
	KindRoute                // an item of http, by its name
	KindDatabase             // a key of postgres.databases
)

// Has tells whether the file, with the edits made so far, holds the entry
// of kind named name. The Set methods add or replace, so a caller that must
// not replace (an add) or must not add (an edit) asks first.
func (f *File) Has(kind Kind, name string) bool {
	switch kind {
	case KindSecret:
		return get(get(f.root, "secrets"), name) != nil
	case KindRoute:
		list := get(f.root, "http")
		return list != nil && list.Kind == yaml.SequenceNode && routeItem(list, name) != nil
	default:
		return get(get(get(f.root, "postgres"), "databases"), name) != nil
	}
}

// Source returns the bytes on disk the file was read from, or last saved.
// The caller must not change them.
func (f *File) Source() []byte { return f.orig }

// Secret returns the command of the secret name, with the edits made so far.
// A missing secret is ErrNotFound; a command that is not a list of strings
// is an error.
func (f *File) Secret(name string) ([]string, error) {
	entry := get(get(f.root, "secrets"), name)
	if entry == nil {
		return nil, fmt.Errorf("secret %q: %w", name, ErrNotFound)
	}
	cmd := get(entry, "command")
	if cmd == nil {
		return nil, nil
	}
	bad := fmt.Errorf("secret %q: the command is not a list of strings", name)
	if cmd.Kind != yaml.SequenceNode {
		return nil, bad
	}
	argv := make([]string, 0, len(cmd.Content))
	for _, a := range cmd.Content {
		if a.Kind != yaml.ScalarNode {
			return nil, bad
		}
		argv = append(argv, a.Value)
	}
	return argv, nil
}

// Route returns listen and upstream of the route name as the file writes
// them, with the edits made so far. A missing route is ErrNotFound.
func (f *File) Route(name string) (listen, upstream string, err error) {
	list := get(f.root, "http")
	var item *yaml.Node
	if list != nil && list.Kind == yaml.SequenceNode {
		item = routeItem(list, name)
	}
	if item == nil {
		return "", "", fmt.Errorf("route %q: %w", name, ErrNotFound)
	}
	return scalar(item, "listen"), scalar(item, "upstream"), nil
}

// Database returns the database alias and postgres.listen as the file writes
// them, with the edits made so far. A missing database is ErrNotFound.
func (f *File) Database(alias string) (d Database, listen string, err error) {
	entry := get(get(get(f.root, "postgres"), "databases"), alias)
	if entry == nil {
		return Database{}, "", fmt.Errorf("database %q: %w", alias, ErrNotFound)
	}
	d = Database{Alias: alias, Upstream: scalar(entry, "upstream"), DBName: scalar(entry, "dbname"),
		User: scalar(entry, "user"), Password: scalar(entry, "password"), RequireAuth: scalar(entry, "require-auth")}
	return d, f.PostgresListen(), nil
}

// PostgresListen returns postgres.listen as the file writes it, or "".
func (f *File) PostgresListen() string { return scalar(get(f.root, "postgres"), "listen") }

// scalar returns the text of the scalar value of key in m, or "".
func scalar(m *yaml.Node, key string) string {
	if v := get(m, key); v != nil && v.Kind == yaml.ScalarNode {
		return v.Value
	}
	return ""
}

func (f *File) RemoveSecret(name string) error {
	return f.removeFromMap("secrets", name)
}

// SetRoute adds the route, or changes listen and upstream of the route with
// that name. Its other keys (transforms) stay.
func (f *File) SetRoute(r Route) error {
	list, err := f.section("http", yaml.SequenceNode)
	if err != nil {
		return err
	}
	item := routeItem(list, r.Name)
	if item == nil {
		item = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		setScalar(item, "name", r.Name)
		list.Content = append(list.Content, item)
	}
	setInt(item, "listen", r.Listen)
	setScalar(item, "upstream", r.Upstream)
	return nil
}

func (f *File) RemoveRoute(name string) error {
	list := get(f.root, "http")
	if list == nil || list.Kind != yaml.SequenceNode {
		return fmt.Errorf("route %q: %w", name, ErrNotFound)
	}
	for i, it := range list.Content {
		if v := get(it, "name"); v != nil && v.Value == name {
			list.Content = append(list.Content[:i], list.Content[i+1:]...)
			if len(list.Content) == 0 {
				f.dropSection("http")
			}
			return nil
		}
	}
	return fmt.Errorf("route %q: %w", name, ErrNotFound)
}

// SetDatabase adds or changes one database entry and sets postgres.listen. An
// empty RequireAuth removes the key.
func (f *File) SetDatabase(listen int, d Database) error {
	pg, err := f.section("postgres", yaml.MappingNode)
	if err != nil {
		return err
	}
	setInt(pg, "listen", listen)
	dbs, err := child(pg, "databases", yaml.MappingNode)
	if err != nil {
		return err
	}
	entry, err := child(dbs, d.Alias, yaml.MappingNode)
	if err != nil {
		return fmt.Errorf("database %q: %w", d.Alias, err)
	}
	setScalar(entry, "upstream", d.Upstream)
	setScalar(entry, "dbname", d.DBName)
	setScalar(entry, "user", d.User)
	setScalar(entry, "password", d.Password)
	if d.RequireAuth == "" {
		deleteKey(entry, "require-auth")
	} else {
		setScalar(entry, "require-auth", d.RequireAuth)
	}
	return nil
}

// RemoveDatabase removes the entry; the last one removes the postgres block.
func (f *File) RemoveDatabase(alias string) error {
	pg := get(f.root, "postgres")
	dbs := (*yaml.Node)(nil)
	if pg != nil && pg.Kind == yaml.MappingNode {
		dbs = get(pg, "databases")
	}
	if dbs == nil || dbs.Kind != yaml.MappingNode || get(dbs, alias) == nil {
		return fmt.Errorf("database %q: %w", alias, ErrNotFound)
	}
	deleteKey(dbs, alias)
	if len(dbs.Content) == 0 {
		f.dropSection("postgres")
	}
	return nil
}

func (f *File) removeFromMap(section, name string) error {
	m := get(f.root, section)
	if m == nil || m.Kind != yaml.MappingNode || get(m, name) == nil {
		return fmt.Errorf("%s %q: %w", strings.TrimSuffix(section, "s"), name, ErrNotFound)
	}
	deleteKey(m, name)
	if len(m.Content) == 0 {
		f.dropSection(section)
	}
	return nil
}

// section returns the top-level value of key, created empty at the end when
// it is absent.
func (f *File) section(key string, kind yaml.Kind) (*yaml.Node, error) {
	return child(f.root, key, kind)
}

// child returns the value of key in m, created empty of the given kind when
// absent. A value of another kind is an error.
func child(m *yaml.Node, key string, kind yaml.Kind) (*yaml.Node, error) {
	if v := get(m, key); v != nil {
		if v.Kind != kind {
			return nil, fmt.Errorf("%q has an unexpected shape", key)
		}
		return v, nil
	}
	tag := "!!map"
	if kind == yaml.SequenceNode {
		tag = "!!seq"
	}
	v := &yaml.Node{Kind: kind, Tag: tag}
	m.Content = append(m.Content, keyNode(key), v)
	return v, nil
}

func routeItem(list *yaml.Node, name string) *yaml.Node {
	for _, it := range list.Content {
		if v := get(it, "name"); v != nil && v.Value == name {
			return it
		}
	}
	return nil
}

func keyNode(key string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
}

// get returns the value of key in mapping m, or nil.
func get(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func setNode(m *yaml.Node, key string, v *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = v
			return
		}
	}
	m.Content = append(m.Content, keyNode(key), v)
}

// setScalar sets key to a string; an existing scalar keeps its comments.
func setScalar(m *yaml.Node, key, value string) {
	setTyped(m, key, "!!str", value)
}

func setInt(m *yaml.Node, key string, value int) {
	setTyped(m, key, "!!int", strconv.Itoa(value))
}

func setTyped(m *yaml.Node, key, tag, value string) {
	if v := get(m, key); v != nil && v.Kind == yaml.ScalarNode {
		v.Tag, v.Value, v.Style = tag, value, 0
		return
	}
	setNode(m, key, &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value})
}

// dropSection removes a top-level key. When it is the first key, its head
// comment may be the file header (yaml.v3 attaches a header without a blank
// line to the first key), so it moves to the document.
func (f *File) dropSection(key string) {
	if f.doc != nil && len(f.root.Content) > 0 && f.root.Content[0].Value == key {
		if hc := f.root.Content[0].HeadComment; hc != "" {
			if f.doc.HeadComment != "" {
				hc = f.doc.HeadComment + "\n\n" + hc
			}
			f.doc.HeadComment = hc
		}
	}
	deleteKey(f.root, key)
}

func deleteKey(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}
