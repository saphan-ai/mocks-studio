package kubemock

import (
	"net/http"
	"strings"
	"testing"
)

func TestObjectCreatedInScopeAIsNotVisibleInScopeB(t *testing.T) {
	t.Parallel()
	r := newRig(t, 0)
	a, b := r.open("scope-a-run"), r.open("scope-b-run")
	if got := a.call(http.MethodPost, cmPath, configMap("acme-example-cm", nil)); got.status != http.StatusCreated {
		t.Fatalf("create in A answered %d: %s", got.status, got.body)
	}
	if got := a.call(http.MethodGet, cmPath+"/acme-example-cm", nil); got.status != http.StatusOK {
		t.Fatalf("the object is not visible in the scope that created it: %d", got.status)
	}
	if got := b.call(http.MethodGet, cmPath+"/acme-example-cm", nil); got.status != http.StatusNotFound {
		t.Fatalf("scope B answered %d for an object created in scope A, want 404", got.status)
	}
	items := b.call(http.MethodGet, cmPath, nil).json(t)["items"].([]any)
	if len(items) != 0 {
		t.Fatalf("scope B lists %d configmaps, want 0", len(items))
	}
}

func TestCreateGetUpdateDeleteRoundTrip(t *testing.T) {
	t.Parallel()
	s := newRig(t, 0).open("crud-roundtrip")
	created := s.call(http.MethodPost, podPath, pod("acme-example-pod"))
	if created.status != http.StatusCreated {
		t.Fatalf("create answered %d: %s", created.status, created.body)
	}
	obj := created.json(t)
	for _, f := range []string{"uid", "resourceVersion", "creationTimestamp"} {
		if str(path(obj, "metadata", f)) == "" {
			t.Errorf("the server did not set metadata.%s", f)
		}
	}
	if str(path(obj, "metadata", "namespace")) != "default" || str(path(obj, "status", "phase")) != "Pending" {
		t.Errorf("namespace or phase not defaulted: %v", obj)
	}
	path(obj, "metadata").(map[string]any)["labels"] = map[string]any{"tier": "gold"}
	updated := s.call(http.MethodPut, podPath+"/acme-example-pod", obj)
	if updated.status != http.StatusOK || str(path(updated.json(t), "metadata", "labels", "tier")) != "gold" {
		t.Fatalf("update answered %d: %s", updated.status, updated.body)
	}
	del := s.call(http.MethodDelete, podPath+"/acme-example-pod", nil)
	if del.status != http.StatusOK || del.json(t)["status"] != "Success" {
		t.Fatalf("delete answered %d: %s", del.status, del.body)
	}
	if got := s.call(http.MethodGet, podPath+"/acme-example-pod", nil); got.status != http.StatusNotFound {
		t.Fatalf("a deleted pod answered %d, want 404", got.status)
	}
}

func TestListFiltersByLabelSelector(t *testing.T) {
	t.Parallel()
	s := newRig(t, 0).open("list-selector")
	for name, tier := range map[string]string{"acme-example-a": "gold", "acme-example-b": "silver", "acme-example-c": "gold"} {
		p := pod(name)
		path(p, "metadata").(map[string]any)["labels"] = map[string]any{"tier": tier}
		s.call(http.MethodPost, podPath, p)
	}
	cases := map[string]int{
		"tier=gold": 2, "tier==silver": 1, "tier!=gold": 1, "tier": 3, "!tier": 0,
		"tier in (gold,silver)": 3, "tier notin (gold)": 1, "tier=gold,tier!=gold": 0, "": 3,
	}
	for sel, want := range cases {
		r := s.call(http.MethodGet, podPath+"?labelSelector="+strings.ReplaceAll(sel, " ", "%20"), nil)
		if got := len(r.json(t)["items"].([]any)); r.status != http.StatusOK || got != want {
			t.Errorf("labelSelector %q returned %d items (status %d), want %d", sel, got, r.status, want)
		}
	}
}

func TestListAcrossNamespacesAndByFieldSelector(t *testing.T) {
	t.Parallel()
	s := newRig(t, 0).open("list-fields")
	s.call(http.MethodPost, listPath, map[string]any{"metadata": map[string]any{"name": "acme-example-ns"}})
	s.call(http.MethodPost, cmPath, configMap("acme-example-a", nil))
	s.call(http.MethodPost, "/api/v1/namespaces/acme-example-ns/configmaps", configMap("acme-example-b", nil))
	all := s.call(http.MethodGet, "/api/v1/configmaps", nil).json(t)["items"].([]any)
	if len(all) != 2 {
		t.Fatalf("a cluster-wide list returned %d configmaps, want 2", len(all))
	}
	one := s.call(http.MethodGet, "/api/v1/configmaps?fieldSelector=metadata.name%3Dacme-example-b", nil).json(t)["items"].([]any)
	if len(one) != 1 {
		t.Fatalf("a metadata.name field selector returned %d configmaps, want 1", len(one))
	}
}

func TestUpdateWithStaleResourceVersionConflictsAndUnchangedUpdateIsNoChange(t *testing.T) {
	t.Parallel()
	s := newRig(t, 0).open("update-rules")
	first := s.call(http.MethodPost, cmPath, configMap("acme-example-cm", map[string]string{"k": "v1"})).json(t)
	same := s.call(http.MethodPut, cmPath+"/acme-example-cm", first)
	if same.status != http.StatusOK || str(path(same.json(t), "metadata", "resourceVersion")) != str(path(first, "metadata", "resourceVersion")) {
		t.Fatalf("an update that changes nothing answered %d and moved the resourceVersion: %s", same.status, same.body)
	}
	changed := s.call(http.MethodPut, cmPath+"/acme-example-cm", map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap", "data": map[string]any{"k": "v2"}, "metadata": first["metadata"],
	}).json(t)
	stale := map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "data": map[string]any{"k": "v3"}, "metadata": first["metadata"]}
	r := s.call(http.MethodPut, cmPath+"/acme-example-cm", stale)
	if r.status != http.StatusConflict || r.json(t)["reason"] != "Conflict" {
		t.Fatalf("an update carrying the old resourceVersion answered %d %s, want 409 Conflict", r.status, r.body)
	}
	if str(path(changed, "metadata", "resourceVersion")) == str(path(first, "metadata", "resourceVersion")) {
		t.Fatal("a real change did not move the resourceVersion")
	}
}

func TestDeletingANamespaceDeletesWhatItHoldsAndTellsTheWatchers(t *testing.T) {
	t.Parallel()
	s := newRig(t, 0).open("namespace-cascade")
	s.call(http.MethodPost, listPath, map[string]any{"metadata": map[string]any{"name": "acme-example-ns"}})
	s.call(http.MethodPost, "/api/v1/namespaces/acme-example-ns/configmaps", configMap("acme-example-cm", nil))
	w := s.watch("/api/v1/configmaps?watch=true&resourceVersion=" + s.listRV("/api/v1/configmaps"))
	if r := s.call(http.MethodDelete, listPath+"/acme-example-ns", nil); r.status != http.StatusOK {
		t.Fatalf("namespace delete answered %d: %s", r.status, r.body)
	}
	if ev := w.mustNext(); ev.Type != "DELETED" || ev.name() != "acme-example-cm" {
		t.Fatalf("watch delivered %s %q, want DELETED acme-example-cm", ev.Type, ev.name())
	}
	if r := s.call(http.MethodGet, "/api/v1/namespaces/acme-example-ns/configmaps/acme-example-cm", nil); r.status != http.StatusNotFound {
		t.Fatalf("a configmap of a deleted namespace answered %d, want 404", r.status)
	}
}

func TestDeploymentReportsItsReplicasAndCountsGenerations(t *testing.T) {
	t.Parallel()
	s := newRig(t, 0).open("deployment-status")
	d := s.call(http.MethodPost, depPath, deployment("acme-example-web", 2)).json(t)
	if str(path(d, "kind")) != "Deployment" || path(d, "status", "readyReplicas") != float64(2) {
		t.Fatalf("deployment not stored with its status: %v", d)
	}
	path(d, "spec").(map[string]any)["replicas"] = 3
	u := s.call(http.MethodPut, depPath+"/acme-example-web", d).json(t)
	if path(u, "metadata", "generation") != float64(2) {
		t.Fatalf("a spec change did not bump metadata.generation: %v", path(u, "metadata"))
	}
}

func TestClientDiscoveryThenListCreateWatch(t *testing.T) {
	t.Parallel()
	s := newRig(t, 0).open("client-flow")
	for p, kind := range map[string]string{"/api": "APIVersions", "/apis": "APIGroupList", "/api/v1": "APIResourceList", "/apis/apps/v1": "APIResourceList", "/apis/apps": "APIGroup"} {
		if r := s.call(http.MethodGet, p, nil); r.status != http.StatusOK || r.json(t)["kind"] != kind {
			t.Errorf("GET %s answered %d %s, want 200 %s", p, r.status, r.body, kind)
		}
	}
	if r := s.call(http.MethodGet, "/version", nil); r.status != http.StatusOK || r.json(t)["gitVersion"] == nil {
		t.Errorf("GET /version answered %d: %s", r.status, r.body)
	}
	if r := s.call(http.MethodGet, "/readyz", nil); r.status != http.StatusOK || string(r.body) != "ok" {
		t.Errorf("GET /readyz answered %d %q", r.status, r.body)
	}
	list := s.call(http.MethodGet, podPath, nil).json(t)
	if list["kind"] != "PodList" || len(list["items"].([]any)) != 0 {
		t.Fatalf("an empty list is not an empty PodList: %v", list)
	}
	w := s.watch(podPath + "?watch=true&resourceVersion=" + str(path(list, "metadata", "resourceVersion")))
	s.call(http.MethodPost, podPath, pod("acme-example-pod"))
	if ev := w.mustNext(); ev.Type != "ADDED" || ev.Object["kind"] != "Pod" {
		t.Fatalf("watch delivered %s of %v", ev.Type, ev.Object["kind"])
	}
}
