package dnsmock

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

// supportedProtocols are the listener protocols a scope may carry; both open the same
// UDP, TCP and DoH listener, and only label the events of the scope.
var supportedProtocols = []string{"http", "dns"}

func (m *Mock) controlMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+controlBase+"/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET "+controlBase+"/manifest", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(m.manifestBytes()) // the client hanging up is not the mock's problem
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
		m.refuse(w, http.StatusBadRequest, "request-invalid", "The scope request is not a JSON object with run_id and listener; send both.", nil)
		return
	}
	if !runIDPattern.MatchString(req.RunID) {
		m.refuse(w, http.StatusUnprocessableEntity, "run-id-invalid", fmt.Sprintf("The run_id %q does not match ^[a-z0-9][a-z0-9._-]{2,63}$ and was refused; send a conforming id.", req.RunID), nil)
		return
	}
	if !contains(supportedProtocols, req.Listener.Protocol) {
		m.refuse(w, http.StatusUnprocessableEntity, "listener-protocol-unsupported", fmt.Sprintf("The listener protocol %q is not one this mock serves and was refused; use one of the allowed protocols.", req.Listener.Protocol), supportedProtocols)
		return
	}
	if !isLoopback(req.Listener.Host) {
		m.refuse(w, http.StatusConflict, "listener-unavailable", fmt.Sprintf("The listener host %q is not loopback and the scope was refused; this mock binds loopback only, so use 127.0.0.1.", req.Listener.Host), nil)
		return
	}
	s, status, code, err := m.openScope(req)
	if err != nil {
		m.refuse(w, status, code, err.Error(), nil)
		return
	}
	m.log.Printf("scope opened run=%s protocol=%s listener=%s", s.runID, s.label, s.addr())
	m.publish(s.runID, "scope.opened", "internal", s.label, "scope opened for run "+s.runID, nil)
	writeJSON(w, http.StatusCreated, map[string]any{"run_id": s.runID, "listener": s.listener})
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func (m *Mock) openScope(req openRequest) (*scope, int, string, error) {
	m.mu.Lock()
	_, dup := m.scopes[req.RunID]
	m.mu.Unlock()
	if dup {
		return nil, http.StatusConflict, "run-already-open", fmt.Errorf("The run %q is already open and the second open was refused; close it first or pick another run_id.", req.RunID)
	}
	s := newScope(m, req.RunID, req.Listener)
	if err := s.start(); err != nil {
		return nil, http.StatusConflict, "listener-unavailable", fmt.Errorf("The listener %s could not be opened and the scope was refused (%v); assign a free port.", s.addr(), err)
	}
	m.mu.Lock()
	if _, dup := m.scopes[req.RunID]; dup { // a concurrent open of the same run won while this one bound
		m.mu.Unlock()
		s.stop()
		return nil, http.StatusConflict, "run-already-open", fmt.Errorf("The run %q is already open and the second open was refused; close it first or pick another run_id.", req.RunID)
	}
	m.scopes[req.RunID] = s
	m.mu.Unlock()
	return s, http.StatusCreated, "", nil
}

func (m *Mock) lookup(w http.ResponseWriter, r *http.Request) *scope {
	id := r.PathValue("run_id")
	m.mu.Lock()
	s := m.scopes[id]
	m.mu.Unlock()
	if s == nil {
		m.refuse(w, http.StatusNotFound, "run-unknown", fmt.Sprintf("No scope is open for run %q and the request was refused; open it first.", id), nil)
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
	m.log.Printf("scope closed run=%s", s.runID)
	m.publish(s.runID, "scope.closed", "internal", s.label, "scope closed for run "+s.runID, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (m *Mock) handleReset(w http.ResponseWriter, r *http.Request) {
	s := m.lookup(w, r)
	if s == nil {
		return
	}
	s.resetSettings()
	m.publish(s.runID, "scope.reset", "internal", s.label, "scope reset to defaults for run "+s.runID, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (m *Mock) handleSettings(w http.ResponseWriter, r *http.Request) {
	s := m.lookup(w, r)
	if s == nil {
		return
	}
	var body map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil || body == nil {
		m.refuse(w, http.StatusBadRequest, "request-invalid", "The settings body is not a JSON object of setting names to values; send one.", nil)
		return
	}
	names := make([]string, 0, len(body))
	for k := range body {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		def, ok := m.defs[name]
		if !ok {
			m.refuse(w, http.StatusUnprocessableEntity, "setting-unknown", fmt.Sprintf("The setting %q is not declared by this mock and the whole request was refused; use one of the allowed settings.", name), sortedNames(m.defs))
			return
		}
		if p := def.check(body[name]); p != nil {
			m.refuse(w, http.StatusUnprocessableEntity, p.code, p.msg, p.allowed)
			return
		}
	}
	snapshot := s.apply(body)
	m.log.Printf("settings set run=%s names=%s", s.runID, strings.Join(names, ","))
	m.publish(s.runID, "scenario.set", "internal", s.label, "settings set: "+strings.Join(names, ", "), nil)
	writeJSON(w, http.StatusOK, map[string]any{"settings": snapshot})
}

func (m *Mock) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	// Subscribed before the headers go out: a client that has the headers may call at once,
	// and an event published before the subscription is lost to it.
	sub := m.subscribe(r.URL.Query().Get("run_id"))
	defer m.unsubscribe(sub)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, ": ready\n\n")
	fl.Flush()
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
