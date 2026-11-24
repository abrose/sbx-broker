// Package config loads and validates the namespace files: one owner-only YAML
// file per namespace.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"

	"github.com/abrose/sbx-broker/internal/placeholder"
)

const (
	minPort = 1024
	maxPort = 65535
)

// knownTransforms is config's own list of transform names. The transform
// package imports config, so config cannot ask its registry.
var knownTransforms = map[string]bool{"swap": true}

type Secret struct {
	Name    string
	Command []string
}

type TransformSpec struct {
	Name     string
	Settings map[string]string
}

type Route struct {
	Name       string
	Listen     int
	Upstream   *url.URL
	Transforms []TransformSpec
}

// Database is one Postgres entry. Password is the name of a secret.
// RequireAuth is "" or RequireAuthSCRAM: the only login method the upstream
// may ask for.
type Database struct{ Alias, Upstream, DBName, User, Password, RequireAuth string }

// RequireAuthSCRAM is the one value of the require-auth key.
const RequireAuthSCRAM = "scram-sha-256"

type Postgres struct {
	Listen    int
	Databases []Database
}

type Namespace struct {
	Name, Path string
	Secrets    []Secret
	HTTP       []Route
	Postgres   *Postgres
}

// Ports returns every listener port: the HTTP routes, then the Postgres port.
func (n *Namespace) Ports() []int {
	var ports []int
	for _, r := range n.HTTP {
		ports = append(ports, r.Listen)
	}
	if n.Postgres != nil {
		ports = append(ports, n.Postgres.Listen)
	}
	return ports
}

// The raw types mirror the YAML. They have no custom unmarshalers, so one
// KnownFields(true) decoder refuses an unknown key at every depth. Only the
// transform list is kept as nodes, because an entry is a string or a map.
type rawFile struct {
	Secrets  map[string]rawSecret `yaml:"secrets"`
	HTTP     []rawRoute           `yaml:"http"`
	Postgres *rawPostgres         `yaml:"postgres"`
}

type rawSecret struct {
	Command []string `yaml:"command"`
}

type rawRoute struct {
	Name       string      `yaml:"name"`
	Listen     int         `yaml:"listen"`
	Upstream   string      `yaml:"upstream"`
	Transforms []yaml.Node `yaml:"transforms"`
}

type rawPostgres struct {
	Listen    int                    `yaml:"listen"`
	Databases map[string]rawDatabase `yaml:"databases"`
}

type rawDatabase struct {
	Upstream string `yaml:"upstream"`
	DBName   string `yaml:"dbname"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	// RequireAuth is a node so that a present-but-empty or null value is not
	// "absent": only a zero Kind means the key was not written.
	RequireAuth yaml.Node `yaml:"require-auth"`
}

// LoadDir reads every *.yaml in dir. ok holds the valid namespaces. bad maps a
// file name to its error. err is set only when dir itself is unusable.
func LoadDir(dir string) (ok map[string]*Namespace, bad map[string]error, err error) {
	if err := checkOwnerOnly(dir, true); err != nil {
		return nil, nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	ok = map[string]*Namespace{}
	bad = map[string]error{}
	portOwner := map[int]string{}
	for _, file := range yamlFiles(entries, "") {
		ns, err := loadFile(filepath.Join(dir, file), strings.TrimSuffix(file, ".yaml"))
		if err == nil {
			err = claimPorts(ns, portOwner)
		}
		if err != nil {
			bad[file] = err
			continue
		}
		ok[ns.Name] = ns
	}
	return ok, bad, nil
}

// Check validates data as the namespace file `file` (<ns>.yaml) of dir, with
// the rules of LoadDir, and checks its ports against every other valid file of
// dir. It does not read `file` from disk, so it works on bytes not yet written.
func Check(dir, file string, data []byte) error {
	name, isYAML := strings.CutSuffix(file, ".yaml")
	if err := checkName(name); err != nil {
		return err
	}
	if !isYAML {
		return fmt.Errorf(invalidName, name)
	}
	ns, err := parse(filepath.Join(dir, file), name, data)
	if err != nil {
		return err
	}
	if err := checkOwnerOnly(dir, true); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	portOwner := map[int]string{}
	for _, o := range yamlFiles(entries, file) {
		other, err := loadFile(filepath.Join(dir, o), strings.TrimSuffix(o, ".yaml"))
		if err == nil {
			_ = claimPorts(other, portOwner) // a clash among the others is not ours
		}
	}
	return claimPorts(ns, portOwner)
}

// claimPorts records the ports of ns, or fails on one held by another namespace.
func claimPorts(ns *Namespace, owner map[int]string) error {
	for _, p := range ns.Ports() {
		if o, taken := owner[p]; taken {
			return fmt.Errorf("port %d is used by namespace %s", p, o)
		}
	}
	for _, p := range ns.Ports() {
		owner[p] = ns.Name
	}
	return nil
}

// CheckOwnerOnlyFile applies to a namespace file the rule LoadDir applies: a
// regular file, not a symlink, owned by the user, not writable by others.
func CheckOwnerOnlyFile(path string) error { return checkOwnerOnly(path, false) }

// CheckOwnerOnlyDir applies to the namespace folder the rule LoadDir applies:
// a directory, not a symlink, owned by the user, not writable by others.
func CheckOwnerOnlyDir(path string) error { return checkOwnerOnly(path, true) }

func checkOwnerOnly(path string, wantDir bool) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return CheckOwnerOnlyInfo(path, fi, wantDir)
}

// CheckOwnerOnlyInfo applies the checks of CheckOwnerOnlyFile (or, with
// wantDir, CheckOwnerOnlyDir) to fi, the info of name. A caller that opened
// the file passes the info of its handle, so the checked file is the read one.
func CheckOwnerOnlyInfo(name string, fi os.FileInfo, wantDir bool) error {
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", name)
	}
	if wantDir && !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", name)
	}
	if !wantDir && !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", name)
	}
	st, isStat := fi.Sys().(*syscall.Stat_t)
	if !isStat {
		return fmt.Errorf("cannot read the owner of %s", name)
	}
	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s is not owned by the current user", name)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by group or others (mode %04o)", name, fi.Mode().Perm())
	}
	return nil
}

// yamlFiles returns the sorted names of the *.yaml entries, without skip.
func yamlFiles(entries []os.DirEntry, skip string) []string {
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".yaml") && e.Name() != skip {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	return files
}

const invalidName = "namespace name %q is invalid (want 1-16 characters of a-z, 0-9)"

// checkName refuses a namespace name LoadDir does not load.
func checkName(name string) error {
	if !placeholder.ValidNamespace(name) {
		return fmt.Errorf(invalidName, name)
	}
	return nil
}

func loadFile(path, name string) (*Namespace, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	if err := checkOwnerOnly(path, false); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parse(path, name, data)
}

// parse decodes and validates the bytes of one namespace file.
func parse(path, name string, data []byte) (*Namespace, error) {
	var raw rawFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return build(path, name, &raw)
}

func build(path, name string, raw *rawFile) (*Namespace, error) {
	ns := &Namespace{Name: name, Path: path}
	secrets, err := buildSecrets(raw.Secrets)
	if err != nil {
		return nil, err
	}
	ns.Secrets = secrets
	ports := map[int]bool{}
	routeNames := map[string]bool{}
	for _, rr := range raw.HTTP {
		r, err := buildRoute(rr)
		if err != nil {
			return nil, err
		}
		if routeNames[r.Name] {
			return nil, fmt.Errorf("duplicate route name %q", r.Name)
		}
		routeNames[r.Name] = true
		if ports[r.Listen] {
			return nil, fmt.Errorf("port %d is used twice in this file", r.Listen)
		}
		ports[r.Listen] = true
		ns.HTTP = append(ns.HTTP, r)
	}
	if raw.Postgres != nil {
		pg, err := buildPostgres(raw.Postgres, secrets)
		if err != nil {
			return nil, err
		}
		if ports[pg.Listen] {
			return nil, fmt.Errorf("port %d is used twice in this file", pg.Listen)
		}
		ns.Postgres = pg
	}
	return ns, nil
}

func buildSecrets(raw map[string]rawSecret) ([]Secret, error) {
	var out []Secret
	for name, rs := range raw {
		if !placeholder.ValidSecretName(name) {
			return nil, fmt.Errorf("secret name %q is invalid", name)
		}
		if len(rs.Command) == 0 || rs.Command[0] == "" {
			return nil, fmt.Errorf("secret %q: command is empty", name)
		}
		out = append(out, Secret{Name: name, Command: rs.Command})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func checkPort(p int) error {
	if p < minPort || p > maxPort {
		return fmt.Errorf("listen port %d is outside %d-%d", p, minPort, maxPort)
	}
	return nil
}

func buildRoute(rr rawRoute) (Route, error) {
	if !placeholder.ValidAlias(rr.Name) {
		return Route{}, fmt.Errorf("route name %q is invalid", rr.Name)
	}
	if err := checkPort(rr.Listen); err != nil {
		return Route{}, fmt.Errorf("route %q: %w", rr.Name, err)
	}
	up, err := parseUpstream(rr.Upstream)
	if err != nil {
		return Route{}, fmt.Errorf("route %q: %w", rr.Name, err)
	}
	transforms, err := buildTransforms(rr.Transforms)
	if err != nil {
		return Route{}, fmt.Errorf("route %q: %w", rr.Name, err)
	}
	return Route{Name: rr.Name, Listen: rr.Listen, Upstream: up, Transforms: transforms}, nil
}

func parseUpstream(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("upstream %q: %w", s, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("upstream %q must be http:// or https:// with a host", s)
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, fmt.Errorf("upstream %q must have no path, query, fragment or user", s)
	}
	return u, nil
}

func buildTransforms(nodes []yaml.Node) ([]TransformSpec, error) {
	if len(nodes) == 0 {
		return []TransformSpec{{Name: "swap", Settings: map[string]string{}}}, nil
	}
	var out []TransformSpec
	for i := range nodes {
		spec, err := parseTransform(&nodes[i])
		if err != nil {
			return nil, err
		}
		if !knownTransforms[spec.Name] {
			return nil, fmt.Errorf("unknown transform %q", spec.Name)
		}
		out = append(out, spec)
	}
	return out, nil
}

func parseTransform(n *yaml.Node) (TransformSpec, error) {
	switch n.Kind {
	case yaml.ScalarNode:
		return TransformSpec{Name: n.Value, Settings: map[string]string{}}, nil
	case yaml.MappingNode:
		if len(n.Content) != 2 {
			return TransformSpec{}, errors.New("a transform entry with settings must have exactly one key")
		}
		settings := map[string]string{}
		if err := n.Content[1].Decode(&settings); err != nil {
			return TransformSpec{}, fmt.Errorf("transform %q settings: %w", n.Content[0].Value, err)
		}
		return TransformSpec{Name: n.Content[0].Value, Settings: settings}, nil
	}
	return TransformSpec{}, errors.New("a transform entry must be a name or a one-key map")
}

func buildPostgres(rp *rawPostgres, secrets []Secret) (*Postgres, error) {
	if err := checkPort(rp.Listen); err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	if len(rp.Databases) == 0 {
		return nil, errors.New("postgres: no databases")
	}
	defined := map[string]bool{}
	for _, s := range secrets {
		defined[s.Name] = true
	}
	pg := &Postgres{Listen: rp.Listen}
	for alias, rd := range rp.Databases {
		if !placeholder.ValidAlias(alias) {
			return nil, fmt.Errorf("postgres: database alias %q is invalid", alias)
		}
		if _, _, err := net.SplitHostPort(rd.Upstream); err != nil {
			return nil, fmt.Errorf("postgres database %q: upstream %q must be host:port", alias, rd.Upstream)
		}
		if rd.DBName == "" || rd.User == "" {
			return nil, fmt.Errorf("postgres database %q: dbname and user are required", alias)
		}
		if !defined[rd.Password] {
			return nil, fmt.Errorf("postgres database %q: password names the undefined secret %q", alias, rd.Password)
		}
		requireAuth := ""
		if rd.RequireAuth.Kind != 0 {
			got := rd.RequireAuth.Value
			if rd.RequireAuth.Kind != yaml.ScalarNode || rd.RequireAuth.Tag == "!!null" || got != RequireAuthSCRAM {
				return nil, fmt.Errorf("postgres database %q: require-auth %q must be %q", alias, got, RequireAuthSCRAM)
			}
			requireAuth = got
		}
		pg.Databases = append(pg.Databases, Database{
			Alias: alias, Upstream: rd.Upstream, DBName: rd.DBName, User: rd.User, Password: rd.Password,
			RequireAuth: requireAuth,
		})
	}
	sort.Slice(pg.Databases, func(i, j int) bool { return pg.Databases[i].Alias < pg.Databases[j].Alias })
	return pg, nil
}
