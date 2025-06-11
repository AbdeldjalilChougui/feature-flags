package server

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/AbdeldjalilChougui/feature-flags/flag"
)

//go:embed web/templates/*.html web/static/htmx.min.js
var webFS embed.FS

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"json": func(v any) string {
		b, _ := json.MarshalIndent(v, "", "  ")
		return string(b)
	},
	"short": func(v any) string {
		b, _ := json.Marshal(v)
		return string(b)
	},
}).ParseFS(webFS, "web/templates/*.html"))

type flagRow struct {
	Flag    *flag.Flag
	Env     string
	Config  *flag.EnvConfig
	Summary string
}

func summarize(c *flag.EnvConfig) string {
	if c == nil {
		return ""
	}
	var parts []string
	if n := len(c.Rules); n > 0 {
		parts = append(parts, plural(n, "rule"))
	}
	if len(c.Default.Rollout) > 0 {
		var ws []string
		for _, w := range c.Default.Rollout {
			ws = append(ws, w.Variant+" "+formatPct(w.Weight)+"%")
		}
		parts = append(parts, "rollout: "+strings.Join(ws, " / "))
	} else {
		parts = append(parts, "serves "+c.Default.Variant)
	}
	return strings.Join(parts, " · ")
}

func plural(n int, w string) string {
	if n == 1 {
		return "1 " + w
	}
	return strconv.Itoa(n) + " " + w + "s"
}

// formatPct turns basis points into a percentage: 2500 → "25", 1050 → "10.5".
func formatPct(bp int) string { return strconv.FormatFloat(float64(bp)/100, 'f', -1, 64) }

func (s *Server) routesUI() {
	admin := func(h http.HandlerFunc) http.Handler { return s.adminAuth(h) }
	// htmx sends "HX-Request: true". A cross-site form can't set custom
	// headers without a CORS preflight, so requiring it on writes blocks
	// CSRF even though Basic-auth credentials are sent automatically.
	write := func(h http.HandlerFunc) http.Handler {
		return s.adminAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("HX-Request") != "true" {
				http.Error(w, "this endpoint is only callable from the admin UI", http.StatusForbidden)
				return
			}
			h(w, r)
		}))
	}
	s.mux.HandleFunc("GET /static/htmx.min.js", func(w http.ResponseWriter, r *http.Request) {
		b, _ := webFS.ReadFile("web/static/htmx.min.js")
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(b)
	})
	s.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/admin", http.StatusFound) })
	s.mux.Handle("GET /admin", admin(s.uiIndex))
	s.mux.Handle("POST /admin/flags", write(s.uiCreate))
	s.mux.Handle("POST /admin/flags/{key}/toggle", write(s.uiToggle))
	s.mux.Handle("GET /admin/flags/{key}", admin(s.uiEdit))
	s.mux.Handle("POST /admin/flags/{key}/config", write(s.uiSaveConfig))
	s.mux.Handle("POST /admin/flags/{key}/test", write(s.uiTest))
	s.mux.Handle("POST /admin/flags/{key}/delete", write(s.uiDelete))
}

func (s *Server) render(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.log.Error("render", "template", name, "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (s *Server) envParam(r *http.Request) string {
	env := r.URL.Query().Get("env")
	envs := s.store.Environments()
	for _, e := range envs {
		if e.Key == env {
			return env
		}
	}
	return envs[0].Key
}

func (s *Server) rows(env string) []flagRow {
	var rows []flagRow
	for _, f := range s.store.Flags() {
		c := f.Environments[env]
		rows = append(rows, flagRow{Flag: f, Env: env, Config: c, Summary: summarize(c)})
	}
	return rows
}

func (s *Server) uiIndex(w http.ResponseWriter, r *http.Request) {
	env := s.envParam(r)
	s.render(w, http.StatusOK, "index.html", map[string]any{
		"Env": env, "Envs": s.store.Environments(), "Rows": s.rows(env), "Version": s.store.Version(),
	})
}

func (s *Server) uiCreate(w http.ResponseWriter, r *http.Request) {
	env := s.envParam(r)
	r.ParseForm()
	f := &flag.Flag{Key: strings.TrimSpace(r.PostForm.Get("key")), Description: strings.TrimSpace(r.PostForm.Get("description")),
		Variants: map[string]any{"on": true, "off": false}}
	_, err := s.store.CreateFlag(f)
	data := map[string]any{"Env": env, "Rows": s.rows(env)}
	if err != nil {
		data["Error"] = err.Error()
	}
	s.render(w, http.StatusOK, "flags", data)
}

func (s *Server) uiToggle(w http.ResponseWriter, r *http.Request) {
	env := s.envParam(r)
	f, err := s.store.UpdateEnv(r.PathValue("key"), env, func(c *flag.EnvConfig) error {
		c.Enabled = !c.Enabled
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), statusFor(err))
		return
	}
	c := f.Environments[env]
	s.render(w, http.StatusOK, "row", flagRow{Flag: f, Env: env, Config: c, Summary: summarize(c)})
}

func (s *Server) uiEdit(w http.ResponseWriter, r *http.Request) {
	env := s.envParam(r)
	f, err := s.store.Flag(r.PathValue("key"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.render(w, http.StatusOK, "edit.html", map[string]any{
		"F": f, "Env": env, "Envs": s.store.Environments(), "Config": f.Environments[env],
		"Ops": strings.Join(flag.Ops, ", "),
	})
}

func (s *Server) uiSaveConfig(w http.ResponseWriter, r *http.Request) {
	env := s.envParam(r)
	r.ParseForm()
	var c flag.EnvConfig
	dec := json.NewDecoder(strings.NewReader(r.PostForm.Get("config")))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		s.render(w, http.StatusOK, "notice", map[string]any{"Error": "Invalid JSON: " + err.Error()})
		return
	}
	_, err := s.store.UpdateEnv(r.PathValue("key"), env, func(cur *flag.EnvConfig) error {
		*cur = c
		return nil
	})
	if err != nil {
		s.render(w, http.StatusOK, "notice", map[string]any{"Error": err.Error()})
		return
	}
	s.render(w, http.StatusOK, "notice", map[string]any{"OK": "Saved. Connected SDKs have been notified."})
}

func (s *Server) uiTest(w http.ResponseWriter, r *http.Request) {
	env := s.envParam(r)
	r.ParseForm()
	var ctx flag.Context
	if err := json.Unmarshal([]byte(r.PostForm.Get("context")), &ctx); err != nil {
		s.render(w, http.StatusOK, "notice", map[string]any{"Error": "Context must be a JSON object: " + err.Error()})
		return
	}
	f, err := s.store.Flag(r.PathValue("key"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ef, _ := f.ForEnv(env)
	s.render(w, http.StatusOK, "result", flag.Evaluate(ef, ctx))
}

func (s *Server) uiDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteFlag(r.PathValue("key")); err != nil && !errors.Is(err, ErrNotFound) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("HX-Redirect", "/admin?env="+s.envParam(r))
	w.WriteHeader(http.StatusOK)
}
