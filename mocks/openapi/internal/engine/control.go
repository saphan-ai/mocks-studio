package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	controlBase    = "/_mock/v1"
	maxControlBody = 64 << 10
)

var runIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,63}$`)

// refusal is the Refusal document of spec §5.
type refusal struct {
	Message    string   `json:"message"`
	ReasonCode string   `json:"reason_code"`
	Allowed    []string `json:"allowed,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n')) // the client hanging up is not the engine's problem
}

func refuse(w http.ResponseWriter, status int, r refusal) { writeJSON(w, status, r) }

// ServeHTTP is the control surface (spec §4). Only the core calls it.
func (e *Engine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.Path, controlBase)
	if !ok || (rest != "" && !strings.HasPrefix(rest, "/")) {
		refuse(w, http.StatusNotFound, refusal{
			Message:    "No control operation lives at this path and the request was refused; the control surface is served under " + controlBase + ".",
			ReasonCode: "control-path-unknown",
		})
		return
	}
	switch parts := strings.Split(strings.TrimPrefix(rest, "/"), "/"); {
	case rest == "/health":
		e.only(w, r, http.MethodGet, func() { writeJSON(w, http.StatusOK, map[string]string{"status": "ok"}) })
	case rest == "/manifest":
		e.only(w, r, http.MethodGet, func() {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(e.manJSON) // the client hanging up is not the engine's problem
		})
	case rest == "/events":
		e.only(w, r, http.MethodGet, func() { e.streamEvents(w, r) })
	case rest == "/scopes":
		e.only(w, r, http.MethodPost, func() { e.handleOpen(w, r) })
	case parts[0] == "scopes" && len(parts) == 2:
		e.only(w, r, http.MethodDelete, func() { e.handleClose(w, parts[1]) })
	case parts[0] == "scopes" && len(parts) == 3 && parts[2] == "reset":
		e.only(w, r, http.MethodPost, func() { e.handleReset(w, parts[1]) })
	case parts[0] == "scopes" && len(parts) == 3 && parts[2] == "settings":
		e.only(w, r, http.MethodPut, func() { e.handleSettings(w, r, parts[1]) })
	default:
		refuse(w, http.StatusNotFound, refusal{
			Message:    "No control operation lives at this path and the request was refused; see the specification's section 4 for the paths.",
			ReasonCode: "control-path-unknown",
		})
	}
}

func (e *Engine) only(w http.ResponseWriter, r *http.Request, method string, next func()) {
	if r.Method != method {
		w.Header().Set("Allow", method)
		refuse(w, http.StatusMethodNotAllowed, refusal{
			Message:    fmt.Sprintf("This control path does not take %s and the request was refused; use %s.", r.Method, method),
			ReasonCode: "method-not-allowed", Allowed: []string{method},
		})
		return
	}
	next()
}

func (e *Engine) scope(run string) *scope {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.scopes[run]
}

func runUnknown(w http.ResponseWriter, run string) {
	refuse(w, http.StatusNotFound, refusal{
		Message:    fmt.Sprintf("No scope is open for the run %q and the request was refused; open one with POST %s/scopes first.", run, controlBase),
		ReasonCode: "run-unknown",
	})
}

func decodeBody(w http.ResponseWriter, r *http.Request, into any) bool {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxControlBody))
	if err == nil {
		dec := json.NewDecoder(bytes.NewReader(data))
		if err = dec.Decode(into); err == nil && dec.More() {
			err = fmt.Errorf("more than one JSON value")
		}
	}
	if err != nil {
		refuse(w, http.StatusUnprocessableEntity, refusal{
			Message:    "The request body is not the single JSON object this operation takes and the request was refused; send one JSON object.",
			ReasonCode: "body-invalid",
		})
		return false
	}
	return true
}

func (e *Engine) handleOpen(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RunID    string       `json:"run_id"`
		Listener listenerSpec `json:"listener"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if !runIDPattern.MatchString(req.RunID) {
		refuse(w, http.StatusUnprocessableEntity, refusal{
			Message:    fmt.Sprintf("The run_id does not match %s and the scope was not opened; use a run id of 3 to 64 lower-case letters, digits, dots, hyphens or underscores.", runIDPattern),
			ReasonCode: "run-id-invalid",
		})
		return
	}
	if req.Listener.Protocol != "http" {
		refuse(w, http.StatusUnprocessableEntity, refusal{
			Message:    fmt.Sprintf("This mock serves only http listeners and the scope was not opened; ask for a listener with protocol %q.", "http"),
			ReasonCode: "listener-protocol-unsupported", Allowed: []string{"http"},
		})
		return
	}
	sc, ref := e.openScope(req.RunID, req.Listener)
	if ref != nil {
		refuse(w, http.StatusConflict, *ref)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"run_id": sc.runID, "listener": sc.listener})
}

func (e *Engine) handleClose(w http.ResponseWriter, run string) {
	e.mu.Lock()
	sc := e.scopes[run]
	delete(e.scopes, run)
	e.mu.Unlock()
	if sc == nil {
		runUnknown(w, run)
		return
	}
	sc.stop() // the listener has stopped answering before the 204
	e.publish(Event{RunID: run, CorrelationID: newTraceID(), CorrelationSource: sourceGenerated, Kind: "scope.closed", Direction: "internal", Summary: "scope closed"})
	e.log.printf("scope closed run=%s", run)
	w.WriteHeader(http.StatusNoContent)
}

func (e *Engine) handleReset(w http.ResponseWriter, run string) {
	sc := e.scope(run)
	if sc == nil {
		runUnknown(w, run)
		return
	}
	sc.resetSettings()
	e.publish(Event{RunID: run, CorrelationID: newTraceID(), CorrelationSource: sourceGenerated, Kind: "scope.reset", Direction: "internal", Summary: "settings returned to their defaults"})
	w.WriteHeader(http.StatusNoContent)
}

func (e *Engine) handleSettings(w http.ResponseWriter, r *http.Request, run string) {
	sc := e.scope(run)
	if sc == nil {
		runUnknown(w, run)
		return
	}
	var body map[string]any
	if !decodeBody(w, r, &body) {
		return
	}
	if ref := e.checkSettings(body); ref != nil {
		refuse(w, http.StatusUnprocessableEntity, *ref)
		return
	}
	all := sc.applySettings(body)
	names := make([]string, 0, len(body))
	for n := range body {
		names = append(names, n)
	}
	sort.Strings(names)
	summary := "settings set:"
	for _, n := range names {
		summary += fmt.Sprintf(" %s=%v", n, body[n])
	}
	e.publish(Event{RunID: run, CorrelationID: newTraceID(), CorrelationSource: sourceGenerated, Kind: "scenario.set", Direction: "internal", Summary: summary})
	writeJSON(w, http.StatusOK, map[string]any{"settings": all})
}

// checkSettings refuses the whole request when any entry is unknown, mistyped or out of range.
func (e *Engine) checkSettings(body map[string]any) *refusal {
	declared := make([]string, 0, len(e.man.Settings))
	for _, s := range e.man.Settings {
		declared = append(declared, s.Name)
	}
	names := make([]string, 0, len(body))
	for n := range body {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		d, ok := e.man.decl(n)
		if !ok {
			return &refusal{
				Message:    fmt.Sprintf("The setting %q is not declared by this mock and the whole request was refused; use one of the allowed settings.", n),
				ReasonCode: "setting-unknown", Allowed: declared,
			}
		}
		f, isNum := body[n].(float64)
		if !isNum || (d.Type == "integer" && f != float64(int64(f))) {
			return &refusal{
				Message:    fmt.Sprintf("The setting %q takes a value of type %s and the whole request was refused; send a value of that type.", n, d.Type),
				ReasonCode: "setting-type-invalid",
			}
		}
		if (d.Minimum != nil && f < *d.Minimum) || (d.Maximum != nil && f > *d.Maximum) {
			return &refusal{
				Message:    fmt.Sprintf("The value %v of the setting %q is outside %v to %v and the whole request was refused; send a value inside that range.", f, n, *d.Minimum, *d.Maximum),
				ReasonCode: "setting-out-of-range",
			}
		}
	}
	return nil
}

// streamEvents serves the event stream (spec §4.6): live events from the moment of connection.
func (e *Engine) streamEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		refuse(w, http.StatusInternalServerError, refusal{Message: "This server cannot stream events and the request was refused; use another transport.", ReasonCode: "stream-unsupported"})
		return
	}
	sub := e.hub.subscribe(r.URL.Query().Get("run_id"))
	defer e.hub.unsubscribe(sub)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, ": connected\n\n") // a failed write ends the loop below on the next one
	fl.Flush()
	tick := time.NewTicker(e.opts.KeepAlive)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case line := <-sub.ch:
			if _, err := fmt.Fprintf(w, "data: %s\n\n", line); err != nil {
				return
			}
		case <-tick.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
		}
		fl.Flush()
	}
}
