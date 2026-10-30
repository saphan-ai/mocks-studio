package kubemock

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const gitVersion = "v0.0.0-acme-example-mock"

type routeKind int

const (
	routeNone routeKind = iota
	routeStatic
	routeCollection
	routeItem
)

// route is what a request path names.
type route struct {
	kind routeKind
	def  *resourceDef
	ns   string
	name string
	doc  any // the body of a static route
}

// safeFields are the payload fields every event of a call may carry in clear.
func (rt route) safeFields() map[string]any {
	if rt.def == nil {
		return nil
	}
	f := map[string]any{"kind": rt.def.kind}
	if rt.ns != "" {
		f["namespace"] = rt.ns
	}
	if rt.name != "" {
		f["name"] = rt.name
	}
	return f
}

func parseRoute(p string) route {
	p = strings.TrimSuffix(p, "/")
	if doc, ok := staticDoc(p); ok {
		return route{kind: routeStatic, doc: doc}
	}
	var group, version string
	var rest []string
	switch {
	case strings.HasPrefix(p, "/api/v1/"):
		version, rest = "v1", strings.Split(strings.TrimPrefix(p, "/api/v1/"), "/")
	case strings.HasPrefix(p, "/apis/apps/v1/"):
		group, version, rest = "apps", "v1", strings.Split(strings.TrimPrefix(p, "/apis/apps/v1/"), "/")
	default:
		return route{}
	}
	if rest[0] == "namespaces" && len(rest) >= 3 {
		def := findDef(group, version, rest[2])
		switch {
		case def == nil || !def.namespaced:
			return route{}
		case len(rest) == 3:
			return route{kind: routeCollection, def: def, ns: rest[1]}
		case len(rest) == 4:
			return route{kind: routeItem, def: def, ns: rest[1], name: rest[3]}
		}
		return route{}
	}
	def := findDef(group, version, rest[0])
	switch {
	case def == nil:
		return route{}
	case len(rest) == 1:
		return route{kind: routeCollection, def: def}
	case len(rest) == 2 && def == namespaceDef:
		return route{kind: routeItem, def: def, name: rest[1]}
	}
	return route{}
}

func findDef(group, version, plural string) *resourceDef {
	for _, d := range allResources {
		if d.group == group && d.version == version && d.plural == plural {
			return d
		}
	}
	return nil
}

var verbs = []string{"create", "delete", "get", "list", "update", "watch"}

func resourceList(group, version string) map[string]any {
	gv := version
	if group != "" {
		gv = group + "/" + version
	}
	var list []any
	for _, d := range allResources {
		if d.group == group && d.version == version {
			list = append(list, map[string]any{"name": d.plural, "singularName": d.singular, "namespaced": d.namespaced, "kind": d.kind, "verbs": verbs})
		}
	}
	return map[string]any{"kind": "APIResourceList", "groupVersion": gv, "resources": list}
}

var appsGroupVersion = map[string]any{"groupVersion": "apps/v1", "version": "v1"}

// staticDoc is the discovery and health answers: they need no cluster state.
func staticDoc(p string) (any, bool) {
	switch p {
	case "/api":
		return map[string]any{"kind": "APIVersions", "versions": []string{"v1"},
			"serverAddressByClientCIDRs": []any{map[string]any{"clientCIDR": "0.0.0.0/0", "serverAddress": ""}}}, true
	case "/apis":
		return map[string]any{"kind": "APIGroupList", "apiVersion": "v1", "groups": []any{appsGroup()}}, true
	case "/apis/apps":
		g := appsGroup()
		g["kind"], g["apiVersion"] = "APIGroup", "v1"
		return g, true
	case "/api/v1":
		return resourceList("", "v1"), true
	case "/apis/apps/v1":
		return resourceList("apps", "v1"), true
	case "/version":
		return map[string]any{"major": "0", "minor": "0", "gitVersion": gitVersion, "platform": "mock/none"}, true
	case "/healthz", "/livez", "/readyz":
		return plainOK{}, true
	}
	return nil, false
}

func appsGroup() map[string]any {
	return map[string]any{"name": "apps", "versions": []any{appsGroupVersion}, "preferredVersion": appsGroupVersion}
}

// plainOK is the text the health endpoints answer.
type plainOK struct{}

func (s *scope) dispatch(c *call, w http.ResponseWriter, r *http.Request, rt route, delayed string) {
	switch rt.kind {
	case routeStatic:
		if r.Method != http.MethodGet {
			s.fail(c, w, errMethod(r.Method, r.URL.Path), delayed)
			return
		}
		if _, ok := rt.doc.(plainOK); ok {
			s.announce(c, http.StatusOK, delayed)
			w.Header().Set("Content-Type", "text/plain")
			_, _ = fmt.Fprint(w, "ok") // the client hanging up is not the mock's problem
			return
		}
		s.reply(c, w, http.StatusOK, rt.doc, delayed)
	case routeCollection:
		s.collection(c, w, r, rt, delayed)
	case routeItem:
		s.item(c, w, r, rt, delayed)
	default:
		s.fail(c, w, errNoRoute(r.URL.Path), delayed)
	}
}

func (s *scope) fail(c *call, w http.ResponseWriter, e *apiError, fault string) {
	s.reply(c, w, e.code, e.status(), fault)
}

func (s *scope) collection(c *call, w http.ResponseWriter, r *http.Request, rt route, fault string) {
	switch r.Method {
	case http.MethodGet:
		label, err := parseSelector(r.URL.Query().Get("labelSelector"))
		if err != nil {
			s.fail(c, w, errBadRequest("unable to parse labelSelector: %v", err), fault)
			return
		}
		field, err := parseFieldSelector(r.URL.Query().Get("fieldSelector"))
		if err != nil {
			s.fail(c, w, errBadRequest("unable to parse fieldSelector: %v", err), fault)
			return
		}
		if watching, _ := strconv.ParseBool(r.URL.Query().Get("watch")); watching {
			s.serveWatch(c, w, r, rt, label, field, fault)
			return
		}
		items, rv := s.cl.list(rt.def, rt.ns, label, field)
		if items == nil {
			items = []map[string]any{}
		}
		s.reply(c, w, http.StatusOK, map[string]any{
			"kind": rt.def.listKind(), "apiVersion": rt.def.apiVersion(),
			"metadata": map[string]any{"resourceVersion": strconv.FormatInt(rv, 10)}, "items": items,
		}, fault)
	case http.MethodPost:
		if rt.def.namespaced && rt.ns == "" {
			s.fail(c, w, errMethod(r.Method, r.URL.Path), fault)
			return
		}
		obj, e := s.decodeObject(c, rt)
		if e != nil {
			s.fail(c, w, e, fault)
			return
		}
		created, e := s.cl.create(rt.def, rt.ns, obj)
		if e != nil {
			s.fail(c, w, e, fault)
			return
		}
		s.noteName(c, created)
		s.reply(c, w, http.StatusCreated, created, fault)
	default:
		s.fail(c, w, errMethod(r.Method, r.URL.Path), fault)
	}
}

func (s *scope) item(c *call, w http.ResponseWriter, r *http.Request, rt route, fault string) {
	switch r.Method {
	case http.MethodGet:
		obj, e := s.cl.get(rt.def, rt.ns, rt.name)
		if e != nil {
			s.fail(c, w, e, fault)
			return
		}
		s.reply(c, w, http.StatusOK, obj, fault)
	case http.MethodPut:
		obj, e := s.decodeObject(c, rt)
		if e != nil {
			s.fail(c, w, e, fault)
			return
		}
		updated, e := s.cl.update(rt.def, rt.ns, rt.name, obj)
		if e != nil {
			s.fail(c, w, e, fault)
			return
		}
		s.reply(c, w, http.StatusOK, updated, fault)
	case http.MethodDelete:
		gone, e := s.cl.remove(rt.def, rt.ns, rt.name)
		if e != nil {
			s.fail(c, w, e, fault)
			return
		}
		s.reply(c, w, http.StatusOK, map[string]any{
			"kind": "Status", "apiVersion": "v1", "metadata": map[string]any{}, "status": "Success",
			"details": map[string]any{"name": rt.name, "group": rt.def.group, "kind": rt.def.plural, "uid": str(path(gone, "metadata", "uid"))},
		}, fault)
	default:
		s.fail(c, w, errMethod(r.Method, r.URL.Path), fault)
	}
}

// decodeObject reads the request body as an object of the route's kind.
func (s *scope) decodeObject(c *call, rt route) (map[string]any, *apiError) {
	var obj map[string]any
	if err := decodeJSON(c.body, &obj); err != nil || obj == nil {
		return nil, errBadRequest("the request body is not a JSON object of kind %s", rt.def.kind)
	}
	if k := str(obj["kind"]); k != "" && k != rt.def.kind {
		return nil, errBadRequest("the kind of the object (%s) does not match the resource of the request (%s)", k, rt.def.kind)
	}
	if v := str(obj["apiVersion"]); v != "" && v != rt.def.apiVersion() {
		return nil, errBadRequest("the apiVersion of the object (%s) does not match the resource of the request (%s)", v, rt.def.apiVersion())
	}
	if rt.def.namespaced {
		if ns := str(path(obj, "metadata", "namespace")); ns != "" && ns != rt.ns {
			return nil, errBadRequest("the namespace of the provided object does not match the namespace sent on the request")
		}
	}
	return obj, nil
}

// noteName records the name a create produced, so its events can carry it.
func (s *scope) noteName(c *call, created map[string]any) {
	if c.fields != nil {
		c.fields["name"] = str(path(created, "metadata", "name"))
	}
}

// serveWatch streams the changes of a collection as one JSON object per line.
func (s *scope) serveWatch(c *call, w http.ResponseWriter, r *http.Request, rt route, label, field selector, fault string) {
	wt, replay, e := s.cl.watch(rt.def, rt.ns, label, field, r.URL.Query().Get("resourceVersion"))
	if e != nil {
		s.fail(c, w, e, fault)
		return
	}
	defer s.cl.unwatch(wt)
	fl, ok := w.(http.Flusher)
	if !ok {
		s.fail(c, w, &apiError{code: http.StatusInternalServerError, reason: "InternalError", message: "the server cannot stream"}, fault)
		return
	}
	dropAfter := s.intSetting(SettingDropWatchAfterEvent)
	var timeout <-chan time.Time
	if secs, err := strconv.Atoi(r.URL.Query().Get("timeoutSeconds")); err == nil && secs > 0 {
		t := time.NewTimer(time.Duration(secs) * time.Second)
		defer t.Stop()
		timeout = t.C
	}
	s.announce(c, http.StatusOK, fault)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	delivered := 0
	send := func(it streamItem) bool {
		if _, err := fmt.Fprintf(w, "{\"type\":%q,\"object\":%s}\n", it.typ, it.raw); err != nil {
			return false
		}
		fl.Flush()
		delivered++
		fields := map[string]any{"kind": it.def.kind, "name": it.name}
		if it.ns != "" {
			fields["namespace"] = it.ns
		}
		s.emit(c, event{Kind: "message.delivered", Direction: "outbound",
			Summary: fmt.Sprintf("watch %s %s delivered at resourceVersion %d", it.typ, it.def.kind, it.rv),
			Payload: map[string]any{"redacted": false, "fields": fields}})
		if dropAfter > 0 && delivered >= dropAfter {
			s.emit(c, event{Kind: "fault.injected", Direction: "internal", Fault: SettingDropWatchAfterEvent,
				Summary: fmt.Sprintf("watch ended after %d event(s) by the setting %s", delivered, SettingDropWatchAfterEvent)})
			return false
		}
		return true
	}
	for _, it := range replay {
		if !send(it) {
			return
		}
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		case <-timeout:
			return
		case it, open := <-wt.ch:
			if !open || !send(it) {
				return
			}
		}
	}
}
