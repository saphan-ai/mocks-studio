package kubemock

import (
	"net/http"
	"testing"
)

// Every test here changes exactly ONE field of a request that is otherwise
// valid, and first proves the unchanged request is accepted, so a refusal
// can only be caused by the field the test names.

func TestStatus404NotFound_OnlyTheNameDiffers(t *testing.T) {
	t.Parallel()
	s := newRig(t, 0).open("status-404-name")
	s.call(http.MethodPost, podPath, pod("acme-example-pod"))
	if r := s.call(http.MethodGet, podPath+"/acme-example-pod", nil); r.status != http.StatusOK {
		t.Fatalf("control: the existing pod answered %d, want 200", r.status)
	}
	r := s.call(http.MethodGet, podPath+"/acme-example-other", nil)
	st := r.json(t)
	d, _ := st["details"].(map[string]any)
	if r.status != http.StatusNotFound || st["reason"] != "NotFound" || st["code"] != float64(404) || d["name"] != "acme-example-other" || d["kind"] != "pods" {
		t.Fatalf("a missing pod answered %d %s, want a 404 NotFound Status naming pods/acme-example-other", r.status, r.body)
	}
}

func TestStatus404NotFound_OnlyTheNamespaceDiffersOnCreate(t *testing.T) {
	t.Parallel()
	s := newRig(t, 0).open("status-404-namespace")
	if r := s.call(http.MethodPost, podPath, pod("acme-example-pod")); r.status != http.StatusCreated {
		t.Fatalf("control: create in the default namespace answered %d, want 201", r.status)
	}
	r := s.call(http.MethodPost, "/api/v1/namespaces/acme-example-missing/pods", pod("acme-example-pod"))
	if r.status != http.StatusNotFound || r.json(t)["reason"] != "NotFound" {
		t.Fatalf("create in a missing namespace answered %d %s, want 404 NotFound", r.status, r.body)
	}
}

func TestStatus409AlreadyExists_OnlyTheNameRepeats(t *testing.T) {
	t.Parallel()
	s := newRig(t, 0).open("status-409-name")
	if r := s.call(http.MethodPost, podPath, pod("acme-example-pod")); r.status != http.StatusCreated {
		t.Fatalf("control: first create answered %d, want 201", r.status)
	}
	if r := s.call(http.MethodPost, podPath, pod("acme-example-other")); r.status != http.StatusCreated {
		t.Fatalf("control: a create with another name answered %d, want 201", r.status)
	}
	r := s.call(http.MethodPost, podPath, pod("acme-example-pod"))
	if r.status != http.StatusConflict || r.json(t)["reason"] != "AlreadyExists" {
		t.Fatalf("a repeated name answered %d %s, want 409 AlreadyExists", r.status, r.body)
	}
}

func TestStatus409Conflict_OnlyTheResourceVersionIsStale(t *testing.T) {
	t.Parallel()
	s := newRig(t, 0).open("status-409-version")
	cur := s.call(http.MethodPost, cmPath, configMap("acme-example-cm", map[string]string{"k": "v"})).json(t)
	cur["data"] = map[string]any{"k": "w"}
	if r := s.call(http.MethodPut, cmPath+"/acme-example-cm", cur); r.status != http.StatusOK {
		t.Fatalf("control: an update with the current resourceVersion answered %d, want 200", r.status)
	}
	cur["data"] = map[string]any{"k": "x"} // same body, now carrying a resourceVersion that is no longer current
	if r := s.call(http.MethodPut, cmPath+"/acme-example-cm", cur); r.status != http.StatusConflict || r.json(t)["reason"] != "Conflict" {
		t.Fatalf("a stale resourceVersion answered %d %s, want 409 Conflict", r.status, r.body)
	}
}

func TestStatus410Expired_OnlyTheResourceVersionIsOld(t *testing.T) {
	t.Parallel()
	s := newRig(t, 2).open("status-410-version")
	for _, n := range []string{"acme-example-a", "acme-example-b", "acme-example-c", "acme-example-d"} {
		s.call(http.MethodPost, cmPath, configMap(n, nil))
	}
	if r := s.call(http.MethodGet, cmPath+"?watch=true&timeoutSeconds=1&resourceVersion="+s.listRV(cmPath), nil); r.status != http.StatusOK {
		t.Fatalf("control: a watch from the current resourceVersion answered %d, want 200", r.status)
	}
	r := s.call(http.MethodGet, cmPath+"?watch=true&resourceVersion=1", nil)
	if r.status != http.StatusGone || r.json(t)["reason"] != "Expired" {
		t.Fatalf("a watch from resourceVersion 1 answered %d %s, want 410 Expired", r.status, r.body)
	}
}

func TestStatus422Invalid_OneFieldBroken(t *testing.T) {
	t.Parallel()
	valid := map[string]func() (string, map[string]any){
		"pod": func() (string, map[string]any) { return podPath, pod("acme-example-ok") },
		"configmap": func() (string, map[string]any) {
			return cmPath, configMap("acme-example-ok", map[string]string{"k": "v"})
		},
		"deployment": func() (string, map[string]any) { return depPath, deployment("acme-example-ok", 1) },
		"namespace": func() (string, map[string]any) {
			return listPath, map[string]any{"metadata": map[string]any{"name": "acme-example-ok"}}
		},
	}
	cases := []struct {
		name, kind, field string
		break_            func(obj map[string]any)
	}{
		{"pod_metadata.name_uppercase", "pod", "metadata.name", func(o map[string]any) { path(o, "metadata").(map[string]any)["name"] = "Acme-Example" }},
		{"pod_metadata.name_missing", "pod", "metadata.name", func(o map[string]any) { delete(path(o, "metadata").(map[string]any), "name") }},
		{"pod_spec.containers_missing", "pod", "spec.containers", func(o map[string]any) { delete(o["spec"].(map[string]any), "containers") }},
		{"pod_spec.containers[0].image_missing", "pod", "spec.containers[0].image", func(o map[string]any) {
			delete(o["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any), "image")
		}},
		{"configmap_data_key_with_space", "configmap", "data[bad key]", func(o map[string]any) { o["data"] = map[string]any{"bad key": "v"} }},
		{"namespace_metadata.name_with_dot", "namespace", "metadata.name", func(o map[string]any) { path(o, "metadata").(map[string]any)["name"] = "acme.example" }},
		{"deployment_spec.replicas_negative", "deployment", "spec.replicas", func(o map[string]any) { o["spec"].(map[string]any)["replicas"] = -1 }},
		{"deployment_spec.selector_missing", "deployment", "spec.selector", func(o map[string]any) { delete(o["spec"].(map[string]any), "selector") }},
		{"deployment_template_labels_differ_from_selector", "deployment", "spec.template.metadata.labels", func(o map[string]any) {
			path(o, "spec", "template", "metadata").(map[string]any)["labels"] = map[string]any{"app": "acme-example-other"}
		}},
		{"deployment_template_containers_missing", "deployment", "spec.template.spec.containers", func(o map[string]any) {
			delete(path(o, "spec", "template", "spec").(map[string]any), "containers")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newRig(t, 0).open("status-422-" + tc.kind)
			target, ok := valid[tc.kind]()
			if r := s.call(http.MethodPost, target, ok); r.status != http.StatusCreated {
				t.Fatalf("control: the valid %s answered %d, want 201: %s", tc.kind, r.status, r.body)
			}
			target, bad := valid[tc.kind]()
			path(bad, "metadata").(map[string]any)["name"] = "acme-example-bad"
			tc.break_(bad)
			r := s.call(http.MethodPost, target, bad)
			st := r.json(t)
			causes, _ := path(st, "details", "causes").([]any)
			if r.status != http.StatusUnprocessableEntity || st["reason"] != "Invalid" || len(causes) == 0 || path(causes[0], "field") != tc.field {
				t.Fatalf("breaking %s answered %d %s, want a 422 Invalid Status whose first cause is %s", tc.name, r.status, r.body, tc.field)
			}
		})
	}
}

func TestStatus400BadRequest_OnlyOneFieldDiffers(t *testing.T) {
	t.Parallel()
	s := newRig(t, 0).open("status-400")
	if r := s.call(http.MethodPost, podPath, pod("acme-example-ok")); r.status != http.StatusCreated {
		t.Fatalf("control: answered %d, want 201", r.status)
	}
	wrongKind := pod("acme-example-kind")
	wrongKind["kind"] = "ConfigMap"
	otherNS := pod("acme-example-ns")
	path(otherNS, "metadata").(map[string]any)["namespace"] = "acme-example-elsewhere"
	for name, tc := range map[string]struct {
		method, p string
		body      any
	}{
		"kind_differs_from_the_resource":     {http.MethodPost, podPath, wrongKind},
		"metadata.namespace_differs_on_path": {http.MethodPost, podPath, otherNS},
		"body_is_not_json":                   {http.MethodPost, podPath, "{not json"},
		"labelSelector_is_malformed":         {http.MethodGet, podPath + "?labelSelector=a%20b%20c", nil},
		"fieldSelector_names_another_field":  {http.MethodGet, podPath + "?fieldSelector=spec.nodeName%3Dx", nil},
		"watch_resourceVersion_not_a_number": {http.MethodGet, podPath + "?watch=true&resourceVersion=abc", nil},
	} {
		r := s.call(tc.method, tc.p, tc.body)
		if r.status != http.StatusBadRequest || r.json(t)["reason"] != "BadRequest" {
			t.Errorf("%s answered %d %s, want 400 BadRequest", name, r.status, r.body)
		}
	}
}

func TestStatus405And404_OnlyTheMethodOrThePathDiffers(t *testing.T) {
	t.Parallel()
	s := newRig(t, 0).open("status-405")
	if r := s.call(http.MethodGet, podPath, nil); r.status != http.StatusOK {
		t.Fatalf("control: list answered %d, want 200", r.status)
	}
	if r := s.call(http.MethodPatch, podPath+"/acme-example-pod", "{}"); r.status != http.StatusMethodNotAllowed || r.json(t)["reason"] != "MethodNotAllowed" {
		t.Errorf("PATCH answered %d %s, want 405 MethodNotAllowed", r.status, r.body)
	}
	if r := s.call(http.MethodGet, nsPath+"/secrets", nil); r.status != http.StatusNotFound || r.json(t)["reason"] != "NotFound" {
		t.Errorf("an unserved resource answered %d %s, want 404 NotFound", r.status, r.body)
	}
}
