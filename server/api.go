package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/AbdeldjalilChougui/feature-flags/flag"
)

// Config for the HTTP server.
type Config struct {
	AdminToken string        // protects /api and /admin; empty = no auth (local only)
	KeepAlive  time.Duration // SSE comment interval (default 20s)
	Logger     *slog.Logger
}

type Server struct {
	cfg   Config
	store *Store
	mux   *http.ServeMux
	log   *slog.Logger
}

func New(st *Store, cfg Config) *Server {
	if cfg.KeepAlive == 0 {
		cfg.KeepAlive = 20 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &Server{cfg: cfg, store: st, mux: http.NewServeMux(), log: cfg.Logger}
	admin := func(h http.HandlerFunc) http.Handler { return s.adminAuth(h) }

	s.mux.Handle("GET /api/environments", admin(s.listEnvs))
	s.mux.Handle("POST /api/environments", admin(s.createEnv))
	s.mux.Handle("GET /api/flags", admin(s.listFlags))
	s.mux.Handle("POST /api/flags", admin(s.createFlag))
	s.mux.Handle("GET /api/flags/{key}", admin(s.getFlag))
	s.mux.Handle("PUT /api/flags/{key}", admin(s.replaceFlag))
	s.mux.Handle("DELETE /api/flags/{key}", admin(s.deleteFlag))
	s.mux.Handle("PUT /api/flags/{key}/environments/{env}", admin(s.putEnvConfig))
	s.mux.Handle("POST /api/flags/{key}/environments/{env}/toggle", admin(s.toggle))
	s.mux.Handle("POST /api/evaluate", admin(s.adminEvaluate))

	s.mux.HandleFunc("GET /sdk/flags", s.sdkFlags)
	s.mux.HandleFunc("GET /sdk/stream", s.sdkStream)
	s.mux.HandleFunc("POST /sdk/evaluate", s.sdkEvaluate)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })

	s.routesUI()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func bearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}

func (s *Server) adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AdminToken == "" {
			next.ServeHTTP(w, r)
			return
		}
		got := bearer(r)
		if got == "" {
			_, got, _ = r.BasicAuth()
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.AdminToken)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="feature-flags"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sdkEnv authenticates an SDK by its environment key (Authorization:
// Bearer, or ?sdk_key= for EventSource, which can't set headers).
func (s *Server) sdkEnv(w http.ResponseWriter, r *http.Request) (Environment, bool) {
	key := bearer(r)
	if key == "" {
		key = r.URL.Query().Get("sdk_key")
	}
	env, ok := s.store.EnvBySDKKey(key)
	if !ok {
		writeErr(w, http.StatusUnauthorized, errors.New("unknown SDK key"))
	}
	return env, ok
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func statusFor(err error) int {
	var ve *ValidationError
	switch {
	case errors.As(err, &ve):
		return http.StatusBadRequest
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrExists):
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("bad JSON: %w", err))
		return false
	}
	return true
}

// ---- admin API ----

func (s *Server) listEnvs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.store.Environments())
}

func (s *Server) createEnv(w http.ResponseWriter, r *http.Request) {
	var in struct{ Key, Name string }
	if !decode(w, r, &in) {
		return
	}
	e, err := s.store.CreateEnvironment(in.Key, in.Name)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, e)
}

func (s *Server) listFlags(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.store.Flags())
}

func (s *Server) createFlag(w http.ResponseWriter, r *http.Request) {
	var f flag.Flag
	if !decode(w, r, &f) {
		return
	}
	out, err := s.store.CreateFlag(&f)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) getFlag(w http.ResponseWriter, r *http.Request) {
	f, err := s.store.Flag(r.PathValue("key"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) replaceFlag(w http.ResponseWriter, r *http.Request) {
	var f flag.Flag
	if !decode(w, r, &f) {
		return
	}
	out, err := s.store.ReplaceFlag(r.PathValue("key"), &f)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) deleteFlag(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteFlag(r.PathValue("key")); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) putEnvConfig(w http.ResponseWriter, r *http.Request) {
	var c flag.EnvConfig
	if !decode(w, r, &c) {
		return
	}
	out, err := s.store.UpdateEnv(r.PathValue("key"), r.PathValue("env"), func(cur *flag.EnvConfig) error {
		*cur = c
		return nil
	})
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, out.Environments[r.PathValue("env")])
}

func (s *Server) toggle(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.UpdateEnv(r.PathValue("key"), r.PathValue("env"), func(c *flag.EnvConfig) error {
		c.Enabled = !c.Enabled
		return nil
	})
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, out.Environments[r.PathValue("env")])
}

type evalRequest struct {
	Env     string       `json:"environment,omitempty"`
	Flag    string       `json:"flag"`
	Context flag.Context `json:"context"`
}

func (s *Server) evaluate(w http.ResponseWriter, env string, in evalRequest) {
	f, err := s.store.Flag(in.Flag)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	ef, ok := f.ForEnv(env)
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("flag %s is not configured in %s", in.Flag, env))
		return
	}
	writeJSON(w, http.StatusOK, flag.Evaluate(ef, in.Context))
}

func (s *Server) adminEvaluate(w http.ResponseWriter, r *http.Request) {
	var in evalRequest
	if !decode(w, r, &in) {
		return
	}
	s.evaluate(w, in.Env, in)
}

// ---- SDK endpoints ----

func (s *Server) sdkFlags(w http.ResponseWriter, r *http.Request) {
	env, ok := s.sdkEnv(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.store.Payload(env.Key))
}

func (s *Server) sdkEvaluate(w http.ResponseWriter, r *http.Request) {
	env, ok := s.sdkEnv(w, r)
	if !ok {
		return
	}
	var in evalRequest
	if !decode(w, r, &in) {
		return
	}
	s.evaluate(w, env.Key, in)
}

// sdkStream is a Server-Sent Events stream. It sends the full payload as a
// "put" event on connect and after every change, and a comment line every
// KeepAlive so proxies don't close an idle connection.
func (s *Server) sdkStream(w http.ResponseWriter, r *http.Request) {
	env, ok := s.sdkEnv(w, r)
	if !ok {
		return
	}
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // nginx: don't buffer
	changes, cancel := s.store.Subscribe()
	defer cancel()

	send := func() error {
		b, _ := json.Marshal(s.store.Payload(env.Key))
		if _, err := fmt.Fprintf(w, "event: put\ndata: %s\n\n", b); err != nil {
			return err
		}
		return rc.Flush()
	}
	w.WriteHeader(http.StatusOK)
	if err := send(); err != nil {
		return
	}
	ping := time.NewTicker(s.cfg.KeepAlive)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-changes:
			if send() != nil {
				return
			}
		case <-ping.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}
