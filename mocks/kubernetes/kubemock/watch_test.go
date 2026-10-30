package kubemock

import (
	"net/http"
	"strconv"
	"testing"
)

func TestWatchOpenedBeforeAChangeReceivesThatChange(t *testing.T) {
	t.Parallel()
	s := newRig(t, 0).open("watch-before")
	rv := s.listRV(cmPath)
	w := s.watch(cmPath + "?watch=true&resourceVersion=" + rv)
	if r := s.call(http.MethodPost, cmPath, configMap("acme-example-cm", map[string]string{"color": "blue"})); r.status != http.StatusCreated {
		t.Fatalf("create answered %d: %s", r.status, r.body)
	}
	ev := w.mustNext()
	if ev.Type != "ADDED" || ev.name() != "acme-example-cm" {
		t.Fatalf("watch delivered %s %q, want ADDED acme-example-cm", ev.Type, ev.name())
	}
	if str(path(ev.Object, "metadata", "resourceVersion")) == "" {
		t.Fatalf("the delivered object carries no resourceVersion: %v", ev.Object)
	}
}

func TestWatchDeliversAddedModifiedDeletedInOrder(t *testing.T) {
	t.Parallel()
	s := newRig(t, 0).open("watch-order")
	w := s.watch(cmPath + "?watch=true&resourceVersion=" + s.listRV(cmPath))
	created := s.call(http.MethodPost, cmPath, configMap("acme-example-cm", map[string]string{"color": "blue"})).json(t)
	created["data"] = map[string]any{"color": "green"}
	if r := s.call(http.MethodPut, cmPath+"/acme-example-cm", created); r.status != http.StatusOK {
		t.Fatalf("update answered %d: %s", r.status, r.body)
	}
	if r := s.call(http.MethodDelete, cmPath+"/acme-example-cm", nil); r.status != http.StatusOK {
		t.Fatalf("delete answered %d: %s", r.status, r.body)
	}
	var types []string
	var last int64
	for range 3 {
		ev := w.mustNext()
		types = append(types, ev.Type)
		rv, _ := strconv.ParseInt(str(path(ev.Object, "metadata", "resourceVersion")), 10, 64)
		if rv <= last {
			t.Fatalf("resourceVersion %d does not grow after %d", rv, last)
		}
		last = rv
	}
	if got := types[0] + " " + types[1] + " " + types[2]; got != "ADDED MODIFIED DELETED" {
		t.Fatalf("watch delivered %s, want ADDED MODIFIED DELETED", got)
	}
}

func TestWatchFromAnOlderResourceVersionReplaysTheMissedChanges(t *testing.T) {
	t.Parallel()
	s := newRig(t, 0).open("watch-replay")
	rv := s.listRV(cmPath)
	for _, n := range []string{"acme-example-a", "acme-example-b"} {
		s.call(http.MethodPost, cmPath, configMap(n, nil))
	}
	w := s.watch(cmPath + "?watch=true&resourceVersion=" + rv)
	if a, b := w.mustNext().name(), w.mustNext().name(); a != "acme-example-a" || b != "acme-example-b" {
		t.Fatalf("replay delivered %s then %s, want a then b", a, b)
	}
}

func TestWatchWithoutResourceVersionStartsWithTheCurrentObjectsAsAdded(t *testing.T) {
	t.Parallel()
	s := newRig(t, 0).open("watch-initial")
	s.call(http.MethodPost, cmPath, configMap("acme-example-cm", nil))
	w := s.watch(cmPath + "?watch=true")
	if ev := w.mustNext(); ev.Type != "ADDED" || ev.name() != "acme-example-cm" {
		t.Fatalf("first event is %s %q, want ADDED acme-example-cm", ev.Type, ev.name())
	}
}

func TestDropWatchAfterEventsEndsTheStreamAfterOneEvent(t *testing.T) {
	t.Parallel()
	s := newRig(t, 0).open("watch-drop")
	s.put(map[string]any{SettingDropWatchAfterEvent: 1})
	w := s.watch(cmPath + "?watch=true&resourceVersion=" + s.listRV(cmPath))
	s.call(http.MethodPost, cmPath, configMap("acme-example-a", nil))
	s.call(http.MethodPost, cmPath, configMap("acme-example-b", nil))
	if ev := w.mustNext(); ev.name() != "acme-example-a" {
		t.Fatalf("first event is %q, want acme-example-a", ev.name())
	}
	if ev, got, ended := w.next(testWait); got || !ended {
		t.Fatalf("the stream did not end after one event: got event %v, ended %v", ev, ended)
	}
	if _, ok := s.event(func(e map[string]any) bool {
		return e["kind"] == "fault.injected" && e["fault"] == SettingDropWatchAfterEvent
	}); !ok {
		t.Fatal("no fault.injected event named drop_watch_after_events")
	}
}

func TestWatchWithDropSettingOffStaysOpenForLaterEvents(t *testing.T) {
	t.Parallel()
	s := newRig(t, 0).open("watch-nodrop")
	w := s.watch(cmPath + "?watch=true&resourceVersion=" + s.listRV(cmPath))
	s.call(http.MethodPost, cmPath, configMap("acme-example-a", nil))
	s.call(http.MethodPost, cmPath, configMap("acme-example-b", nil))
	w.mustNext()
	if ev := w.mustNext(); ev.name() != "acme-example-b" {
		t.Fatalf("second event is %q, want acme-example-b", ev.name())
	}
}

func TestWatchWithTooOldResourceVersionAnswersGone(t *testing.T) {
	t.Parallel()
	const window = 3
	s := newRig(t, window).open("watch-gone")
	for i := range 6 {
		s.call(http.MethodPost, cmPath, configMap("acme-example-"+strconv.Itoa(i), nil))
	}
	// Control: the one field that differs is resourceVersion; the current one is inside the window.
	inside := s.call(http.MethodGet, cmPath+"?watch=true&timeoutSeconds=1&resourceVersion="+s.listRV(cmPath), nil)
	if inside.status != http.StatusOK {
		t.Fatalf("a watch from the current resourceVersion answered %d, want 200: %s", inside.status, inside.body)
	}
	old := s.call(http.MethodGet, cmPath+"?watch=true&resourceVersion=1", nil)
	st := old.json(t)
	if old.status != http.StatusGone || st["code"] != float64(410) || st["kind"] != "Status" || st["status"] != "Failure" {
		t.Fatalf("a watch from resourceVersion 1 answered %d %v, want a 410 Status", old.status, st)
	}
}

func TestWatchByLabelSelectorSeesAnObjectEnterAndLeaveTheSelection(t *testing.T) {
	t.Parallel()
	s := newRig(t, 0).open("watch-selector")
	w := s.watch(cmPath + "?watch=true&labelSelector=tier%3Dgold&resourceVersion=" + s.listRV(cmPath))
	cm := configMap("acme-example-cm", nil)
	s.call(http.MethodPost, cmPath, cm) // no label: outside the selection, so no event
	cur := s.call(http.MethodGet, cmPath+"/acme-example-cm", nil).json(t)
	path(cur, "metadata").(map[string]any)["labels"] = map[string]any{"tier": "gold"}
	cur = s.call(http.MethodPut, cmPath+"/acme-example-cm", cur).json(t)
	if ev := w.mustNext(); ev.Type != "ADDED" {
		t.Fatalf("an object that gained the label arrived as %s, want ADDED", ev.Type)
	}
	path(cur, "metadata").(map[string]any)["labels"] = map[string]any{"tier": "silver"}
	s.call(http.MethodPut, cmPath+"/acme-example-cm", cur)
	if ev := w.mustNext(); ev.Type != "DELETED" {
		t.Fatalf("an object that lost the label arrived as %s, want DELETED", ev.Type)
	}
}

func TestWatchOfOneScopeNeverSeesAnotherScopesChange(t *testing.T) {
	t.Parallel()
	r := newRig(t, 0)
	a, b := r.open("scope-a-run"), r.open("scope-b-run")
	w := b.watch(cmPath + "?watch=true&resourceVersion=" + b.listRV(cmPath))
	a.call(http.MethodPost, cmPath, configMap("acme-example-in-a", nil))
	if ev, got, _ := w.next(testNoMore); got {
		t.Fatalf("scope B's watch received %s %q, which was created in scope A", ev.Type, ev.name())
	}
}
