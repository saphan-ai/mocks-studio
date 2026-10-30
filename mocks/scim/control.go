package scimmock

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

// settingDef is one setting as the manifest declares it; the mock enforces what it declares.
type settingDef struct {
	Name     string
	Type     string
	Default  any
	Allowed  []any
	Min, Max *float64
}

func parseManifest(raw []byte) ([]settingDef, string, error) {
	var doc struct {
		Version  string `json:"version"`
		Settings []struct {
			Name    string   `json:"name"`
			Type    string   `json:"type"`
			Default any      `json:"default"`
			Allowed []any    `json:"allowed"`
			Min     *float64 `json:"minimum"`
			Max     *float64 `json:"maximum"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, "", fmt.Errorf("the embedded manifest is not valid JSON: %w", err)
	}
	out := make([]settingDef, 0, len(doc.Settings))
	for _, s := range doc.Settings {
		out = append(out, settingDef{Name: s.Name, Type: s.Type, Default: s.Default, Allowed: s.Allowed, Min: s.Min, Max: s.Max})
	}
	return out, doc.Version, nil
}

func (m *Mock) controlMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+controlBase+"/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET "+controlBase+"/manifest", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(m.Manifest()) // the client hanging up is not the mock's problem
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

func (m *Mock) handleOpenScope(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RunID    string       `json:"run_id"`
		Listener listenerSpec `json:"listener"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		refuse(w, http.StatusBadRequest, "request-invalid", "The scope request is not a JSON object with run_id and listener and was refused; send both.", nil)
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
	if !isLoopback(req.Listener.Host) {
		refuse(w, http.StatusConflict, "listener-unavailable", fmt.Sprintf("The listener host %q is not a loopback address and the scope was refused; this mock serves loopback only, assign 127.0.0.1.", req.Listener.Host), nil)
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
	if _, dup := m.scopes[req.RunID]; dup { // two opens of one run raced; the first wins
		m.mu.Unlock()
		s.stop()
		refuse(w, http.StatusConflict, "run-already-open", fmt.Sprintf("The run %q is already open and the second open was refused; close it first or pick another run_id.", req.RunID), nil)
		return
	}
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
		refuse(w, http.StatusBadRequest, "request-invalid", "The settings body is not a JSON object of setting names to values and was refused; send one.", nil)
		return
	}
	names := make([]string, 0, len(body))
	for k := range body {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		def, ok := m.def(name)
		if !ok {
			refuse(w, http.StatusUnprocessableEntity, "setting-unknown", fmt.Sprintf("The setting %q is not declared by this mock and the whole request was refused; use one of the allowed settings.", name), m.declaredNames())
			return
		}
		if code, msg, allowed := def.check(body[name]); code != "" {
			refuse(w, http.StatusUnprocessableEntity, code, msg, allowed)
			return
		}
	}
	s.apply(body)
	m.publish(s.runID, "scenario.set", "internal", "settings set: "+strings.Join(names, ", "), nil)
	writeJSON(w, http.StatusOK, map[string]any{"settings": s.snapshot()})
}

func (m *Mock) def(name string) (settingDef, bool) {
	for _, d := range m.declared {
		if d.Name == name {
			return d, true
		}
	}
	return settingDef{}, false
}

func (m *Mock) declaredNames() []string {
	out := make([]string, 0, len(m.declared))
	for _, d := range m.declared {
		out = append(out, d.Name)
	}
	sort.Strings(out)
	return out
}

func (d settingDef) check(v any) (code, msg string, allowed []string) {
	if !typeMatches(d.Type, v) {
		return "setting-type-invalid", fmt.Sprintf("The value of %q is not of type %s and the request was refused; send a %s.", d.Name, d.Type, d.Type), nil
	}
	if f, ok := v.(float64); ok {
		if d.Min != nil && f < *d.Min || d.Max != nil && f > *d.Max {
			return "setting-out-of-range", fmt.Sprintf("The value %v of %q is outside %v..%v and the request was refused; send a value inside the range.", f, d.Name, *d.Min, *d.Max), nil
		}
		if d.Name == "error_status" && f >= 1 && f < 100 {
			return "setting-out-of-range", fmt.Sprintf("The value %v of %q is not an HTTP status and the request was refused; send 0 to turn it off or a status from 100 to 599.", f, d.Name), nil
		}
	}
	if len(d.Allowed) > 0 {
		list := make([]string, len(d.Allowed))
		hit := false
		for i, a := range d.Allowed {
			list[i] = fmt.Sprint(a)
			hit = hit || a == v
		}
		if !hit {
			return "setting-out-of-range", fmt.Sprintf("The value %v of %q is not an allowed value and the request was refused; use one of the allowed values.", v, d.Name), list
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
