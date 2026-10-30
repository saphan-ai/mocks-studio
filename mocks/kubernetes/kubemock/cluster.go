package kubemock

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Watch event types.
const (
	evAdded    = "ADDED"
	evModified = "MODIFIED"
	evDeleted  = "DELETED"
)

const (
	defaultNamespaceName = "default"
	watcherBuffer        = 256
	timestampFormat      = "2006-01-02T15:04:05Z"
)

// change is one entry of the replay window.
type change struct {
	rv        int64
	typ       string
	def       *resourceDef
	ns, name  string
	raw       []byte
	oldLabels map[string]string // labels before the change (nil for an addition)
	newLabels map[string]string // labels after the change (the last ones, for a deletion)
}

// streamItem is one event a watch delivers.
type streamItem struct {
	typ  string
	def  *resourceDef
	ns   string
	name string
	rv   int64
	raw  []byte
}

type watcher struct {
	def   *resourceDef
	ns    string // "" watches every namespace
	label selector
	field selector
	ch    chan streamItem
}

// cluster is one scope's in-memory state. Two scopes never share one.
type cluster struct {
	mu        sync.Mutex
	rv        int64
	retain    int
	objects   map[string]map[string]map[string]any // plural -> "ns/name" -> object
	history   []change
	compacted int64 // the newest resource version that fell out of the window
	watchers  map[*watcher]struct{}
	closed    bool
}

func newCluster(retain int) *cluster {
	c := &cluster{retain: retain, objects: map[string]map[string]map[string]any{}, watchers: map[*watcher]struct{}{}}
	for _, d := range allResources {
		c.objects[d.plural] = map[string]map[string]any{}
	}
	if _, err := c.create(namespaceDef, "", map[string]any{"metadata": map[string]any{"name": defaultNamespaceName}}); err != nil {
		panic(err) // the seed object is valid by construction
	}
	return c
}

func objectKey(ns, name string) string { return ns + "/" + name }

// decodeJSON decodes with UseNumber so integers survive a round trip unchanged.
func decodeJSON(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	return dec.Decode(v)
}

func deepCopy(obj map[string]any) map[string]any {
	b, err := json.Marshal(obj)
	if err != nil {
		panic(err) // objects hold only JSON values
	}
	var out map[string]any
	if err := decodeJSON(b, &out); err != nil {
		panic(err)
	}
	return out
}

func marshal(obj map[string]any) []byte {
	b, err := json.Marshal(obj)
	if err != nil {
		panic(err) // objects hold only JSON values
	}
	return b
}

func newUID() string {
	h := randomHex(16)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// create stores a new object. The caller holds no lock.
func (c *cluster) create(d *resourceDef, ns string, in map[string]any) (map[string]any, *apiError) {
	obj := deepCopy(in)
	meta := metadata(obj)
	if str(meta["name"]) == "" && str(meta["generateName"]) != "" {
		meta["name"] = generateName(str(meta["generateName"]))
	}
	name := str(meta["name"])
	obj["apiVersion"], obj["kind"] = d.apiVersion(), d.kind
	if causes := d.validate(obj); len(causes) > 0 {
		return nil, errInvalid(d, name, causes)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if d.namespaced {
		if _, ok := c.objects[namespaceDef.plural][objectKey("", ns)]; !ok {
			return nil, errNotFound(namespaceDef, ns)
		}
		meta["namespace"] = ns
	}
	key := objectKey(ns, name)
	if _, dup := c.objects[d.plural][key]; dup {
		return nil, errAlreadyExists(d, name)
	}
	delete(meta, "generateName")
	delete(meta, "selfLink")
	meta["uid"] = newUID()
	meta["creationTimestamp"] = time.Now().UTC().Format(timestampFormat)
	if d.kind == "Deployment" || d.kind == "Pod" {
		meta["generation"] = int64(1)
	}
	if d.defaults != nil {
		d.defaults(obj)
	}
	c.rv++
	meta["resourceVersion"] = strconv.FormatInt(c.rv, 10)
	c.objects[d.plural][key] = obj
	c.record(evAdded, d, ns, name, obj, nil)
	return deepCopy(obj), nil
}

func (c *cluster) get(d *resourceDef, ns, name string) (map[string]any, *apiError) {
	c.mu.Lock()
	defer c.mu.Unlock()
	obj, ok := c.objects[d.plural][objectKey(ns, name)]
	if !ok {
		return nil, errNotFound(d, name)
	}
	return deepCopy(obj), nil
}

// list returns the matching objects, ordered by namespace and name, and the
// resource version the list is consistent with.
func (c *cluster) list(d *resourceDef, ns string, label, field selector) ([]map[string]any, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.matching(d, ns, label, field), c.rv
}

func (c *cluster) matching(d *resourceDef, ns string, label, field selector) []map[string]any {
	keys := make([]string, 0, len(c.objects[d.plural]))
	for k := range c.objects[d.plural] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []map[string]any
	for _, k := range keys {
		obj := c.objects[d.plural][k]
		meta := obj["metadata"].(map[string]any)
		if ns != "" && str(meta["namespace"]) != ns {
			continue
		}
		if label.matches(labelsOf(obj)) && field.matches(fieldsOf(obj)) {
			out = append(out, deepCopy(obj))
		}
	}
	return out
}

func fieldsOf(obj map[string]any) map[string]string {
	meta, _ := obj["metadata"].(map[string]any)
	return map[string]string{"metadata.name": str(meta["name"]), "metadata.namespace": str(meta["namespace"])}
}

// update replaces an object. A resourceVersion in the body, when present, must
// be the stored one; a replacement that changes nothing is not a change.
func (c *cluster) update(d *resourceDef, ns, name string, in map[string]any) (map[string]any, *apiError) {
	obj := deepCopy(in)
	meta := metadata(obj)
	obj["apiVersion"], obj["kind"] = d.apiVersion(), d.kind
	if got := str(meta["name"]); got != name {
		return nil, errBadRequest("the name of the object (%s) does not match the name on the URL (%s)", got, name)
	}
	if d.namespaced && str(meta["namespace"]) != "" && str(meta["namespace"]) != ns {
		return nil, errBadRequest("the namespace of the provided object does not match the namespace sent on the request")
	}
	if causes := d.validate(obj); len(causes) > 0 {
		return nil, errInvalid(d, name, causes)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := objectKey(ns, name)
	old, ok := c.objects[d.plural][key]
	if !ok {
		return nil, errNotFound(d, name)
	}
	oldMeta := old["metadata"].(map[string]any)
	if rv := str(meta["resourceVersion"]); rv != "" && rv != str(oldMeta["resourceVersion"]) {
		return nil, errConflict(d, name)
	}
	for _, k := range []string{"uid", "creationTimestamp", "namespace", "generation", "resourceVersion"} {
		if v, ok := oldMeta[k]; ok {
			meta[k] = v
		} else {
			delete(meta, k)
		}
	}
	delete(meta, "generateName")
	delete(meta, "selfLink")
	if d.defaults != nil {
		d.defaults(obj)
	}
	if st, ok := old["status"]; ok {
		obj["status"] = st
	}
	if d.kind == "Namespace" {
		obj["spec"] = old["spec"]
	}
	if d.kind == "Deployment" && !sameJSON(old["spec"], obj["spec"]) {
		gen, _ := intValue(oldMeta["generation"])
		meta["generation"] = gen + 1
		spec, _ := obj["spec"].(map[string]any)
		obj["status"] = deploymentStatus(spec, gen+1)
	}
	if sameJSON(old, obj) {
		return deepCopy(old), nil
	}
	c.rv++
	meta["resourceVersion"] = strconv.FormatInt(c.rv, 10)
	c.objects[d.plural][key] = obj
	c.record(evModified, d, ns, name, obj, old)
	return deepCopy(obj), nil
}

func sameJSON(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return bytes.Equal(ab, bb)
}

// remove deletes an object; deleting a namespace deletes what it holds first.
func (c *cluster) remove(d *resourceDef, ns, name string) (map[string]any, *apiError) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.objects[d.plural][objectKey(ns, name)]; !ok {
		return nil, errNotFound(d, name)
	}
	if d == namespaceDef {
		for _, inner := range allResources {
			if !inner.namespaced {
				continue
			}
			for _, o := range c.matching(inner, name, nil, nil) {
				c.deleteLocked(inner, name, str(path(o, "metadata", "name")))
			}
		}
	}
	return c.deleteLocked(d, ns, name), nil
}

func (c *cluster) deleteLocked(d *resourceDef, ns, name string) map[string]any {
	key := objectKey(ns, name)
	obj := c.objects[d.plural][key]
	delete(c.objects[d.plural], key)
	c.rv++
	final := deepCopy(obj)
	metadata(final)["resourceVersion"] = strconv.FormatInt(c.rv, 10)
	c.record(evDeleted, d, ns, name, final, obj)
	return final
}

// record appends a change to the window and hands it to the watchers. The caller holds c.mu.
func (c *cluster) record(typ string, d *resourceDef, ns, name string, obj, old map[string]any) {
	ch := change{rv: c.rv, typ: typ, def: d, ns: ns, name: name, raw: marshal(obj)}
	if typ != evDeleted {
		ch.newLabels = labelsOf(obj)
	}
	if old != nil {
		ch.oldLabels = labelsOf(old)
	}
	c.history = append(c.history, ch)
	for len(c.history) > c.retain {
		c.compacted = c.history[0].rv
		c.history = c.history[1:]
	}
	for w := range c.watchers {
		item, ok := w.deliver(ch)
		if !ok {
			continue
		}
		select {
		case w.ch <- item:
		default: // a watcher that cannot keep up is cut off; its client re-lists
			delete(c.watchers, w)
			close(w.ch)
		}
	}
}

// deliver decides what, if anything, a watcher with this selector sees of a change.
func (w *watcher) deliver(ch change) (streamItem, bool) {
	if ch.def != w.def || (w.ns != "" && w.ns != ch.ns) {
		return streamItem{}, false
	}
	fields := map[string]string{"metadata.name": ch.name, "metadata.namespace": ch.ns}
	if !w.field.matches(fields) {
		return streamItem{}, false
	}
	typ := ch.typ
	switch ch.typ {
	case evAdded:
		if !w.label.matches(ch.newLabels) {
			return streamItem{}, false
		}
	case evDeleted:
		if !w.label.matches(ch.oldLabels) {
			return streamItem{}, false
		}
	default:
		was, is := w.label.matches(ch.oldLabels), w.label.matches(ch.newLabels)
		switch {
		case was && is:
		case is:
			typ = evAdded
		case was:
			typ = evDeleted
		default:
			return streamItem{}, false
		}
	}
	return streamItem{typ: typ, def: ch.def, ns: ch.ns, name: ch.name, rv: ch.rv, raw: ch.raw}, true
}

// watch opens a watch. With no resourceVersion (or "0") it first replays the
// current objects as ADDED; with one, it replays every retained change after
// it, or answers 410 when that version has fallen out of the window.
func (c *cluster) watch(d *resourceDef, ns string, label, field selector, resourceVersion string) (*watcher, []streamItem, *apiError) {
	var from int64
	if resourceVersion != "" {
		v, err := strconv.ParseInt(resourceVersion, 10, 64)
		if err != nil || v < 0 {
			return nil, nil, errBadRequest("resourceVersion %q is not a non-negative integer", resourceVersion)
		}
		from = v
	}
	w := &watcher{def: d, ns: ns, label: label, field: field, ch: make(chan streamItem, watcherBuffer)}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, nil, &apiError{code: 503, reason: "ServiceUnavailable", message: "the scope is closed"}
	}
	var replay []streamItem
	if from == 0 {
		for _, obj := range c.matching(d, ns, label, field) {
			meta := obj["metadata"].(map[string]any)
			rv, _ := strconv.ParseInt(str(meta["resourceVersion"]), 10, 64)
			replay = append(replay, streamItem{typ: evAdded, def: d, ns: str(meta["namespace"]), name: str(meta["name"]), rv: rv, raw: marshal(obj)})
		}
	} else {
		if from < c.compacted {
			return nil, nil, errGone(from, c.compacted)
		}
		for _, ch := range c.history {
			if ch.rv <= from {
				continue
			}
			if item, ok := w.deliver(ch); ok {
				replay = append(replay, item)
			}
		}
	}
	c.watchers[w] = struct{}{}
	return w, replay, nil
}

func (c *cluster) unwatch(w *watcher) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.watchers[w]; ok {
		delete(c.watchers, w)
		close(w.ch)
	}
}

// close ends every watch; the cluster is not used afterwards.
func (c *cluster) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for w := range c.watchers {
		delete(c.watchers, w)
		close(w.ch)
	}
}

// currentRV is the newest resource version, for list metadata.
func (c *cluster) currentRV() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strconv.FormatInt(c.rv, 10)
}
