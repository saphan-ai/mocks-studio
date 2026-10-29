package suite

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/saphan-ai/mocks-studio/conformance/internal/schemas"
)

func (e *env) checkScopeOpens(protocol string) outcome {
	h, r := e.open(protocol)
	if h == nil {
		return fail("POST /_mock/v1/scopes for a %s listener: %s, want 201", protocol, describeReply(r))
	}
	defer e.closeScope(h)
	doc, ok := r.json()
	if !ok || doc["run_id"] != h.runID {
		return fail("the 201 body does not echo run_id %q: %s", h.runID, truncate(string(r.body), 200))
	}
	if _, has := doc["listener"].(map[string]any); !has {
		return fail("the 201 body carries no listener object: %s", truncate(string(r.body), 200))
	}
	return pass("a scope opened for run %s on %s", h.runID, h.addr())
}

func (e *env) checkListenerAnswers(protocol string) outcome {
	h, done, bad := e.openScoped(protocol)
	if bad != nil {
		return *bad
	}
	defer done()
	p := e.probe(h, probeOpts{})
	if p.err != nil {
		return fail("the %s listener %s opened for run %s did not answer: %v", protocol, h.addr(), h.runID, p.err)
	}
	return pass("the %s listener %s answered a probe", protocol, h.addr())
}

// refusalProblem validates a refusal reply: status, document shape, reason code.
func refusalProblem(r reply, status int, code string) (map[string]any, string) {
	if r.err != nil {
		return nil, "no answer (" + r.err.Error() + ")"
	}
	if r.status != status {
		return nil, fmt.Sprintf("status %d, want %d (body %s)", r.status, status, truncate(string(r.body), 160))
	}
	doc, ok := r.json()
	if !ok {
		return nil, "the refusal body is not a JSON Refusal document: " + truncate(string(r.body), 160)
	}
	problem, err := schemas.Validate(schemas.RefusalID, doc)
	if err != nil {
		return nil, err.Error()
	}
	if problem != "" {
		return doc, "the refusal is not a Refusal document: " + problem
	}
	if doc["reason_code"] != code {
		return doc, fmt.Sprintf("reason_code is %v, want %s", doc["reason_code"], code)
	}
	return doc, ""
}

func (e *env) checkInvalidRunID(protocol string) outcome {
	port, err := e.freePort()
	if err != nil {
		return fail("no free port could be found: %v", err)
	}
	const invalid = "A1"
	r := e.call(http.MethodPost, controlBase+"/scopes", e.openRequest(invalid, protocol, port))
	if r.status == http.StatusCreated {
		e.closeScope(&scopeHandle{runID: invalid})
	}
	if _, problem := refusalProblem(r, http.StatusUnprocessableEntity, "run-id-invalid"); problem != "" {
		return fail("a scope request with run_id %q: %s", invalid, problem)
	}
	return pass("run_id %q was refused with 422 run-id-invalid", invalid)
}

func (e *env) checkDuplicateScope(protocol string) outcome {
	h, done, bad := e.openScoped(protocol)
	if bad != nil {
		return *bad
	}
	defer done()
	port, err := e.freePort()
	if err != nil {
		return fail("no free port could be found: %v", err)
	}
	r := e.call(http.MethodPost, controlBase+"/scopes", e.openRequest(h.runID, protocol, port))
	if _, problem := refusalProblem(r, http.StatusConflict, "run-already-open"); problem != "" {
		return fail("a second open of run %s: %s", h.runID, problem)
	}
	if p := e.probe(h, probeOpts{}); p.err != nil {
		return fail("after the refused second open, the first scope's listener stopped answering: %v", p.err)
	}
	return pass("a second open of run %s was refused with 409 run-already-open and the first scope kept answering", h.runID)
}

func (e *env) checkUnsupportedProtocol() outcome {
	var undeclared string
	for _, p := range []string{"http", "tcp", "smtp", "dns", "other"} {
		if !contains(e.manifest.Protocols, p) {
			undeclared = p
			break
		}
	}
	if undeclared == "" {
		return notApplicable("the mock declares every protocol there is")
	}
	port, err := e.freePort()
	if err != nil {
		return fail("no free port could be found: %v", err)
	}
	runID := e.newRunID()
	r := e.call(http.MethodPost, controlBase+"/scopes", e.openRequest(runID, undeclared, port))
	if r.status == http.StatusCreated {
		e.closeScope(&scopeHandle{runID: runID})
	}
	doc, problem := refusalProblem(r, http.StatusUnprocessableEntity, "listener-protocol-unsupported")
	if problem != "" {
		return fail("a listener of undeclared protocol %s: %s", undeclared, problem)
	}
	if got := stringList(doc["allowed"]); !sameSet(got, e.manifest.Protocols) {
		return fail("the refusal's allowed list is [%s], want the declared protocols [%s]", joinList(got), joinList(e.manifest.Protocols))
	}
	return pass("a %s listener was refused with 422 listener-protocol-unsupported and the declared protocols", undeclared)
}

func (e *env) checkListenerClosed(protocol string) outcome {
	h, r := e.open(protocol)
	if h == nil {
		return fail("a scope could not be opened for the check: %s", describeReply(r))
	}
	if p := e.probe(h, probeOpts{}); p.err != nil {
		e.closeScope(h)
		return fail("the listener never answered before the scope was closed: %v", p.err)
	}
	d := e.call(http.MethodDelete, controlBase+"/scopes/"+h.runID, nil)
	if d.err != nil || d.status != http.StatusNoContent {
		return fail("DELETE /_mock/v1/scopes/%s: %s, want 204", h.runID, describeReply(d))
	}
	if p := e.probe(h, probeOpts{}); p.err == nil {
		return fail("the %s listener %s still answers after its scope was closed", protocol, h.addr())
	}
	return pass("DELETE answered 204 and the listener %s no longer answers", h.addr())
}

func stringList(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, fmt.Sprint(it))
	}
	return out
}

func sameSet(a, b []string) bool {
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	return strings.Join(x, "\x00") == strings.Join(y, "\x00")
}
