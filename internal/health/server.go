package health

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/fsykk/new-api-bot/internal/newapi"
	"github.com/fsykk/new-api-bot/internal/qq"
	"github.com/fsykk/new-api-bot/internal/store"
)

type Server struct {
	Store   *store.Store
	NewAPI  *newapi.Client
	QQ      *qq.Client
	Gateway *qq.Gateway
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.ready)
	return mux
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	if err := s.Store.Ping(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unhealthy", "database": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	checks := map[string]any{}
	ready := true
	if err := s.Store.Ping(); err != nil {
		checks["database"] = err.Error()
		ready = false
	} else {
		checks["database"] = "ok"
	}
	if !s.Gateway.Connected() {
		checks["qq_gateway"] = "disconnected"
		ready = false
	} else {
		checks["qq_gateway"] = "connected"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if _, err := s.QQ.AccessToken(ctx); err != nil {
		checks["qq_token"] = err.Error()
		ready = false
	} else {
		checks["qq_token"] = "ok"
	}
	if _, err := s.NewAPI.GetStatus(ctx, false); err != nil {
		checks["new_api"] = err.Error()
		ready = false
	} else {
		checks["new_api"] = "ok"
	}
	status := http.StatusOK
	state := "ready"
	if !ready {
		status = http.StatusServiceUnavailable
		state = "not_ready"
	}
	writeJSON(w, status, map[string]any{"status": state, "checks": checks})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
