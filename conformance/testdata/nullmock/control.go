package nullmock

import (
	"encoding/json"
	"fmt"
	"math"
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
	mux.HandleFunc("GET "+controlBase+"/health", m.handleHealth)
	mux.HandleFunc("GET "+controlBase+"/manifest", m.handleManifest)
	mux.HandleFunc("POST "+controlBase+"/scopes", m.handleOpenScope)
	mux.HandleFunc("DELETE "+controlBase+"/scopes/{run_id}", m.handleCloseScope)
	mux.HandleFunc("POST "+controlBase+"/scopes/{run_id}/reset", m.handleReset)
	mux.HandleFunc("PUT "+controlBase+"/scopes/{run_id}/settings", m.handleSettings)
	mux.HandleFunc("GET "+controlBase+"/events", m.handleEvents)
	return mux
}

func (m *Mock) handleHealth(w http.ResponseWriter, _ *http.Request) {
	if m.isMutant(MutantHealthFailing) {
		m.refuse(w, http.StatusInternalServerError, "unhealthy", "The mock reports itself unhealthy and refuses to serve; restart it.", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (m *Mock) handleManifest(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(m.Manifest()) // the client hanging up is not the fixture's problem
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
	if m.isMutant(MutantScopeOpenRefused) {
		m.refuse(w, http.StatusInternalServerError, "scope-unavailable", "The mock cannot open scopes at the moment; try again later.", nil)
		return
	}
	if !runIDPattern.MatchString(req.RunID) && !m.isMutant(MutantAcceptsInvalidRunID) {
		m.refuse(w, http.StatusUnprocessableEntity, "run-id-invalid", fmt.Sprintf("The run_id %q does not match ^[a-z0-9][a-z0-9._-]{2,63}$ and was refused; send a conforming id.", req.RunID), nil)
		return
	}
	want := m.cfg.Mode
	if req.Listener.Protocol != want && !m.isMutant(MutantAcceptsUnsupportedProto) {
		m.refuse(w, http.StatusUnprocessableEntity, "listener-protocol-unsupported", fmt.Sprintf("The listener protocol %q is not one this mock serves and was refused; use one of the allowed protocols.", req.Listener.Protocol), []string{want})
		return
	}
	s, status, err := m.openScope(req)
	if err != nil {
		code := "listener-unavailable"
		if status == http.StatusConflict && strings.Contains(err.Error(), "already open") {
			code = "run-already-open"
		}
		m.refuse(w, status, code, err.Error(), nil)
		return
	}
	m.publish(s.runID, "scope.opened", "internal", s.protocol, "scope opened for run "+s.runID, nil)
	writeJSON(w, http.StatusCreated, map[string]any{"run_id": s.runID, "listener": s.listener})
}

func (m *Mock) openScope(req openRequest) (*scope, int, error) {
	m.mu.Lock()
	if old, dup := m.scopes[req.RunID]; dup {
		if !m.isMutant(MutantMergesDuplicateScope) {
			m.mu.Unlock()
			return nil, http.StatusConflict, fmt.Errorf("The run %q is already open and the second open was refused; close it first or pick another run_id.", req.RunID)
		}
		delete(m.scopes, req.RunID)
		m.mu.Unlock()
		old.stop()
		return m.openScope(req)
	}
	m.mu.Unlock()
	s := newScope(m, req.RunID, req.Listener)
	if err := s.start(); err != nil {
		return nil, http.StatusConflict, fmt.Errorf("The listener %s:%d could not be opened and the scope was refused (%v); assign a free port.", req.Listener.Host, req.Listener.Port, err)
	}
	m.mu.Lock()
	m.scopes[req.RunID] = s
	m.order = append(m.order, req.RunID)
	m.mu.Unlock()
	return s, http.StatusCreated, nil
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
	for i, id := range m.order {
		if id == s.runID {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	m.mu.Unlock()
	if !m.isMutant(MutantListenerStaysOpen) {
		s.stop()
	}
	if !m.isMutant(MutantNoScopeClosedEvent) {
		m.publish(s.runID, "scope.closed", "internal", s.protocol, "scope closed for run "+s.runID, nil)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (m *Mock) handleReset(w http.ResponseWriter, r *http.Request) {
	s := m.lookup(w, r)
	if s == nil {
		return
	}
	if !m.isMutant(MutantResetKeepsSettings) {
		s.resetSettings()
	}
	if !m.isMutant(MutantNoScopeResetEvent) {
		m.publish(s.runID, "scope.reset", "internal", s.protocol, "scope reset to defaults for run "+s.runID, nil)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (m *Mock) handleSettings(w http.ResponseWriter, r *http.Request) {
	s := m.lookup(w, r)
	if s == nil {
		return
	}
	var body map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil || body == nil {
		m.refuse(w, http.StatusBadRequest, "request-invalid", "The settings body is not a JSON object of setting names to values; send one.", nil)
		return
	}
	names := make([]string, 0, len(body))
	for k := range body {
		names = append(names, k)
	}
	sort.Strings(names)
	declared := s.declared()
	for _, name := range names {
		def, ok := declared[name]
		if !ok {
			if m.isMutant(MutantUnknownSettingAccepted) {
				delete(body, name)
				continue
			}
			m.refuse(w, http.StatusUnprocessableEntity, "setting-unknown", fmt.Sprintf("The setting %q is not declared by this mock and the whole request was refused; use one of the allowed settings.", name), sortedKeys(declared))
			return
		}
		if code, msg, allowed := def.check(name, body[name], m); code != "" {
			m.refuse(w, http.StatusUnprocessableEntity, code, msg, allowed)
			return
		}
	}
	s.apply(body, m.isMutant(MutantSettingNoEffect))
	if !m.isMutant(MutantNoScenarioSetEvent) {
		m.publish(s.runID, "scenario.set", "internal", s.protocol, "settings set: "+quoteJoin(names), nil)
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": s.snapshot()})
}

func (m *Mock) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ctype := "text/event-stream"
	if m.isMutant(MutantEventsWrongContentType) {
		ctype = "application/json"
	}
	w.Header().Set("Content-Type", ctype)
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

// setting is the fixture's own view of one declared setting.
type setting struct {
	typ      string
	def      any
	allowed  []any
	min, max *float64
}

func (d setting) check(name string, v any, m *Mock) (code, msg string, allowed []string) {
	if !m.isMutant(MutantWrongTypeAccepted) && !typeMatches(d.typ, v) {
		return "setting-type-invalid", fmt.Sprintf("The value of %q is not of type %s and the request was refused; send a %s.", name, d.typ, d.typ), nil
	}
	if m.isMutant(MutantOutOfRangeAccepted) {
		return "", "", nil
	}
	if f, ok := v.(float64); ok && d.min != nil && d.max != nil && (f < *d.min || f > *d.max) {
		return "setting-out-of-range", fmt.Sprintf("The value %v of %q is outside %v..%v and the request was refused; send a value inside the range.", f, name, *d.min, *d.max), nil
	}
	if len(d.allowed) > 0 {
		list := make([]string, len(d.allowed))
		hit := false
		for i, a := range d.allowed {
			list[i] = fmt.Sprint(a)
			hit = hit || a == v
		}
		if !hit {
			return "setting-out-of-range", fmt.Sprintf("The value %v of %q is not an allowed value and the request was refused; use one of the allowed values.", v, name), list
		}
	}
	return "", "", nil
}

func typeMatches(typ string, v any) bool {
	switch typ {
	case "integer":
		f, ok := v.(float64)
		return ok && f == math.Trunc(f)
	case "number":
		_, ok := v.(float64)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	default:
		_, ok := v.(bool)
		return ok
	}
}

func sortedKeys(m map[string]setting) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
