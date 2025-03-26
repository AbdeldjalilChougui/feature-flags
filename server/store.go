// Package server is the flag server: storage, REST API, SSE streaming to
// SDKs and the htmx admin UI.
package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"sync"

	"github.com/AbdeldjalilChougui/feature-flags/flag"
)

// Environment is a deployment stage with its own SDK key.
type Environment struct {
	Key    string `json:"key"`
	Name   string `json:"name"`
	SDKKey string `json:"sdk_key"`
}

// State is everything the server stores. It is small, so it is kept in
// memory and persisted as one JSON file written atomically.
type State struct {
	Version      int64                 `json:"version"`
	Environments []Environment         `json:"environments"`
	Flags        map[string]*flag.Flag `json:"flags"`
}

// Payload is what an SDK receives for its environment.
type Payload struct {
	Version     int64                   `json:"version"`
	Environment string                  `json:"environment"`
	Flags       map[string]flag.EnvFlag `json:"flags"`
}

var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
	envKeyRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
)

// ValidationError wraps errors caused by bad input (HTTP 400).
type ValidationError struct{ Err error }

func (e *ValidationError) Error() string { return e.Err.Error() }
func (e *ValidationError) Unwrap() error { return e.Err }

type Store struct {
	mu   sync.RWMutex
	path string // "" = memory only
	st   State
	subs map[chan struct{}]struct{}
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Open loads path or creates a fresh state with "development" and
// "production" environments.
func Open(path string) (*Store, error) {
	s := &Store{path: path, subs: map[chan struct{}]struct{}{}}
	if path != "" {
		b, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := json.Unmarshal(b, &s.st); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
		case !os.IsNotExist(err):
			return nil, err
		}
	}
	if s.st.Flags == nil {
		s.st.Flags = map[string]*flag.Flag{}
	}
	if len(s.st.Environments) == 0 {
		s.st.Environments = []Environment{
			{Key: "development", Name: "Development", SDKKey: "sdk-" + randomHex(16)},
			{Key: "production", Name: "Production", SDKKey: "sdk-" + randomHex(16)},
		}
		if err := s.save(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) save() error {
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Subscribe returns a channel that receives a signal after every change.
// Signals coalesce: a slow reader just sees "something changed".
func (s *Store) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}
}

// commit bumps the version, persists, and notifies. Called with mu held.
func (s *Store) commit() error {
	s.st.Version++
	if err := s.save(); err != nil {
		return err
	}
	for ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	return nil
}

func clone[T any](v T) T {
	b, _ := json.Marshal(v)
	var out T
	json.Unmarshal(b, &out)
	return out
}

// ---- reads ----

func (s *Store) Version() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.st.Version
}

func (s *Store) Environments() []Environment {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Environment(nil), s.st.Environments...)
}

// EnvBySDKKey finds the environment an SDK key belongs to.
func (s *Store) EnvBySDKKey(key string) (Environment, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.st.Environments {
		if key != "" && e.SDKKey == key {
			return e, true
		}
	}
	return Environment{}, false
}

func (s *Store) hasEnv(key string) bool {
	for _, e := range s.st.Environments {
		if e.Key == key {
			return true
		}
	}
	return false
}

// Flags returns copies, sorted by key.
func (s *Store) Flags() []*flag.Flag {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*flag.Flag, 0, len(s.st.Flags))
	for _, f := range s.st.Flags {
		out = append(out, clone(f))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func (s *Store) Flag(key string) (*flag.Flag, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f, ok := s.st.Flags[key]
	if !ok {
		return nil, ErrNotFound
	}
	return clone(f), nil
}

// Payload builds the SDK view of one environment.
func (s *Store) Payload(env string) Payload {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p := Payload{Version: s.st.Version, Environment: env, Flags: map[string]flag.EnvFlag{}}
	for k, f := range s.st.Flags {
		if ef, ok := f.ForEnv(env); ok {
			p.Flags[k] = clone(ef)
		}
	}
	return p
}

// ---- writes ----

// defaultConfig is the config a flag gets in an environment nobody has
// configured yet: off, serving the first variant (sorted) when turned on.
func defaultConfig(f *flag.Flag) *flag.EnvConfig {
	names := make([]string, 0, len(f.Variants))
	for n := range f.Variants {
		names = append(names, n)
	}
	sort.Strings(names)
	off, on := names[0], names[0]
	if _, ok := f.Variants["off"]; ok {
		off = "off"
	}
	if _, ok := f.Variants["on"]; ok {
		on = "on"
	}
	return &flag.EnvConfig{Enabled: false, OffVariant: off, Default: flag.Serve{Variant: on}}
}

func (s *Store) prepare(f *flag.Flag) error {
	if f.Salt == "" {
		f.Salt = randomHex(8)
	}
	if f.Environments == nil {
		f.Environments = map[string]*flag.EnvConfig{}
	}
	for env := range f.Environments {
		if !s.hasEnv(env) {
			return &ValidationError{fmt.Errorf("unknown environment %q", env)}
		}
	}
	if len(f.Variants) == 0 {
		return &ValidationError{errors.New("a flag needs at least one variant")}
	}
	for _, e := range s.st.Environments {
		if f.Environments[e.Key] == nil {
			f.Environments[e.Key] = defaultConfig(f)
		}
	}
	if err := f.Validate(); err != nil {
		return &ValidationError{err}
	}
	return nil
}

// CreateFlag adds a flag; unconfigured environments get a default (off) config.
func (s *Store) CreateFlag(f *flag.Flag) (*flag.Flag, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f = clone(f)
	if _, ok := s.st.Flags[f.Key]; ok {
		return nil, ErrExists
	}
	if err := s.prepare(f); err != nil {
		return nil, err
	}
	s.st.Flags[f.Key] = f
	return clone(f), s.commit()
}

// ReplaceFlag overwrites a flag definition (keeping its salt unless given).
func (s *Store) ReplaceFlag(key string, f *flag.Flag) (*flag.Flag, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.st.Flags[key]
	if !ok {
		return nil, ErrNotFound
	}
	f = clone(f)
	f.Key = key
	if f.Salt == "" {
		f.Salt = old.Salt
	}
	if err := s.prepare(f); err != nil {
		return nil, err
	}
	s.st.Flags[key] = f
	return clone(f), s.commit()
}

// UpdateEnv changes one environment's config of a flag via fn.
func (s *Store) UpdateEnv(key, env string, fn func(c *flag.EnvConfig) error) (*flag.Flag, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.st.Flags[key]
	if !ok || !s.hasEnv(env) {
		return nil, ErrNotFound
	}
	nf := clone(f)
	c := nf.Environments[env]
	if c == nil {
		c = defaultConfig(nf)
		nf.Environments[env] = c
	}
	if err := fn(c); err != nil {
		return nil, &ValidationError{err}
	}
	if err := c.Validate(nf.Variants); err != nil {
		return nil, &ValidationError{err}
	}
	s.st.Flags[key] = nf
	return clone(nf), s.commit()
}

func (s *Store) DeleteFlag(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.st.Flags[key]; !ok {
		return ErrNotFound
	}
	delete(s.st.Flags, key)
	return s.commit()
}

// CreateEnvironment adds an environment; every flag starts off there.
func (s *Store) CreateEnvironment(key, name string) (Environment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !envKeyRe.MatchString(key) {
		return Environment{}, &ValidationError{fmt.Errorf("environment key %q must be lowercase letters, digits, '-' or '_'", key)}
	}
	if s.hasEnv(key) {
		return Environment{}, ErrExists
	}
	if name == "" {
		name = key
	}
	e := Environment{Key: key, Name: name, SDKKey: "sdk-" + randomHex(16)}
	s.st.Environments = append(s.st.Environments, e)
	for _, f := range s.st.Flags {
		f.Environments[key] = defaultConfig(f)
	}
	return e, s.commit()
}
