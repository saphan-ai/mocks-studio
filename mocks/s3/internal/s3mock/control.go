package s3mock

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

var runIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,63}$`)

// sseKeepAlive is how often an idle event stream sends a comment line.
const sseKeepAlive = 15 * time.Second

func (m *Mock) controlMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+controlBase+"/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET "+controlBase+"/manifest", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(ManifestBytes(m.controlURL)) // the client hanging up is not the mock's problem
	})
	mux.HandleFunc("POST "+controlBase+"/scopes", m.handleOpenScope)
	mux.HandleFunc("DELETE "+controlBase+"/scopes/{run_id}", m.handleCloseScope)
	mux.HandleFunc("POST "+controlBase+"/scopes/{run_id}/reset", m.handleReset)
	mux.HandleFunc("PUT "+controlBase+"/scopes/{run_id}/settings", m.handleSettings)
	mux.HandleFunc("GET "+controlBase+"/events", m.handleEvents)
	return mux
}

type listenerSpec struct {
	Protocol string `json:"protocol"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
}

type openRequest struct {
	RunID    string       `json:"run_id"`
	Listener listenerSpec `json:"listener"`
}

func (m *Mock) handleOpenScope(w http.ResponseWriter, r *http.Request) {
	var req openRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		refuse(w, http.StatusBadRequest, "request-invalid", "The scope request is not a JSON object with run_id and listener; send both.", nil)
		return
	}
	if !runIDPattern.MatchString(req.RunID) {
		refuse(w, http.StatusUnprocessableEntity, "run-id-invalid", fmt.Sprintf("The run_id %q does not match ^[a-z0-9][a-z0-9._-]{2,63}$ and was refused; send a conforming id.", req.RunID), nil)
		return
	}
	if req.Listener.Protocol != "http" {
		refuse(w, http.StatusUnprocessableEntity, "listener-protocol-unsupported", fmt.Sprintf("The listener protocol %q is not one this mock serves and was refused; use one of the allowed protocols.", req.Listener.Protocol), []string{"http"})
		return
	}
	m.mu.Lock()
	if _, dup := m.scopes[req.RunID]; dup {
		m.mu.Unlock()
		refuse(w, http.StatusConflict, "run-already-open", fmt.Sprintf("The run %q is already open and the second open was refused; close it first or pick another run_id.", req.RunID), nil)
		return
	}
	m.mu.Unlock()
	s := newScope(m, req.RunID, req.Listener)
	if err := s.start(); err != nil {
		refuse(w, http.StatusConflict, "listener-unavailable", fmt.Sprintf("The listener %s could not be opened and the scope was refused (%v); assign a free port.", s.addr(), err), nil)
		return
	}
	m.mu.Lock()
	m.scopes[req.RunID] = s
	m.mu.Unlock()
	m.publish(s.runID, "scope.opened", "internal", "scope opened for run "+s.runID, nil)
	writeJSON(w, http.StatusCreated, map[string]any{"run_id": s.runID, "listener": s.listener})
}

func (m *Mock) lookup(w http.ResponseWriter, r *http.Request) *scope {
	id := r.PathValue("run_id")
	m.mu.Lock()
	s := m.scopes[id]
	m.mu.Unlock()
	if s == nil {
		refuse(w, http.StatusNotFound, "run-unknown", fmt.Sprintf("No scope is open for run %q and the request was refused; open it first.", id), nil)
	}
	return s
}

func (m *Mock) handleCloseScope(w http.ResponseWriter, r *http.Request) {
	s := m.lookup(w, r)
	if s == nil {
		return
	}
	m.mu.Lock()
	delete(m.scopes, s.runID)
	m.mu.Unlock()
	s.stop()
	m.publish(s.runID, "scope.closed", "internal", "scope closed for run "+s.runID, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (m *Mock) handleReset(w http.ResponseWriter, r *http.Request) {
	s := m.lookup(w, r)
	if s == nil {
		return
	}
	s.resetSettings()
	m.publish(s.runID, "scope.reset", "internal", "scope reset to defaults for run "+s.runID, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (m *Mock) handleSettings(w http.ResponseWriter, r *http.Request) {
	s := m.lookup(w, r)
	if s == nil {
		return
	}
	var body map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil || body == nil {
		refuse(w, http.StatusBadRequest, "request-invalid", "The settings body is not a JSON object of setting names to values; send one.", nil)
		return
	}
	names := make([]string, 0, len(body))
	for k := range body {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		def, ok := defByName(name)
		if !ok {
			refuse(w, http.StatusUnprocessableEntity, "setting-unknown", fmt.Sprintf("The setting %q is not declared by this mock and the whole request was refused; use one of the allowed settings.", name), settingNames())
			return
		}
		if code, msg, allowed := def.check(body[name]); code != "" {
			refuse(w, http.StatusUnprocessableEntity, code, msg, allowed)
			return
		}
	}
	s.apply(body)
	// Names only: a setting's value may be the scope's secret.
	m.publish(s.runID, "scenario.set", "internal", "settings set: "+strings.Join(names, ", "), nil)
	writeJSON(w, http.StatusOK, map[string]any{"settings": s.snapshot()})
}

func (m *Mock) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, ": ready\n\n")
	fl.Flush()
	sub := m.subscribe(r.URL.Query().Get("run_id"))
	defer m.unsubscribe(sub)
	tick := time.NewTicker(sseKeepAlive)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			_, _ = fmt.Fprint(w, ": keep-alive\n\n")
			fl.Flush()
		case line := <-sub.ch:
			_, _ = fmt.Fprintf(w, "data: %s\n\n", line)
			fl.Flush()
		}
	}
}
