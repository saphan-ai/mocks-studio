package ocimock

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/http"
	"os"
	"testing"
)

var update = flag.Bool("update", false, "rewrite mocks.manifest.json from the code")

const committedManifestPath = "../../mocks.manifest.json"

func mustJSON(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
}

// The file an operator keeps and the document the mock serves are one document.
func TestCommittedManifestEqualsServed(t *testing.T) {
	want := ManifestBytes("http://127.0.0.1:9100")
	if *update {
		if err := os.WriteFile(committedManifestPath, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(committedManifestPath)
	if err != nil {
		t.Fatalf("read the committed manifest: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("mocks.manifest.json differs from what the code renders for http://127.0.0.1:9100; run go test -run TestCommittedManifestEqualsServed -update")
	}
	e := newEnv(t)
	res := e.control(http.MethodGet, "/manifest", nil)
	if !bytes.Equal(res.body, ManifestBytes(e.m.ControlURL())) {
		t.Error("the served manifest is not the rendered one")
	}
}

func TestSettingsAreAllOrNothing(t *testing.T) {
	e := newEnv(t)
	res := e.control(http.MethodPut, "/scopes/"+e.runID+"/settings", map[string]any{settingAuthRequired: true, settingRateLimit: -1})
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("a body with one bad entry: %d, want 422", res.status)
	}
	if e.do(http.MethodGet, "/v2/", nil).status != http.StatusOK {
		t.Error("auth_required was applied although the request was refused")
	}
	bad := e.control(http.MethodPut, "/scopes/"+e.runID+"/settings", map[string]any{settingMissingBlob: "sha256:short"})
	if bad.status != http.StatusUnprocessableEntity {
		t.Errorf("a missing_blob entry that is not a digest: %d, want 422", bad.status)
	}
}

func TestFaultInjectedEventNamesTheSetting(t *testing.T) {
	for _, tc := range []struct {
		setting string
		value   any
		call    func(e *env, im image) result
	}{
		{settingAuthRequired, true, func(e *env, _ image) result { return e.do(http.MethodGet, "/v2/", nil) }},
		{settingRateLimit, 1, func(e *env, _ image) result {
			e.do(http.MethodGet, "/v2/", nil)
			return e.do(http.MethodGet, "/v2/", nil)
		}},
		{settingMissingBlob, "set-below", func(e *env, im image) result {
			return e.do(http.MethodGet, "/v2/"+testRepo+"/blobs/"+im.layerDigest, nil)
		}},
	} {
		t.Run(tc.setting, func(t *testing.T) {
			e := newEnv(t)
			im := buildImage("layer")
			e.pushImage(testRepo, "v1", im)
			if tc.value == "set-below" {
				tc.value = im.layerDigest
			}
			e.set(map[string]any{tc.setting: tc.value})
			e.allEvents() // discard what the push emitted
			e.events = nil
			if res := tc.call(e, im); res.status < 400 {
				t.Fatalf("the injected fault did not answer an error: %d", res.status)
			}
			found := false
			for _, ev := range e.allEvents() {
				if ev["kind"] == "fault.injected" && ev["fault"] == tc.setting && ev["outcome"] == "fault" {
					found = true
				}
			}
			if !found {
				t.Errorf("no fault.injected event naming %s in %s", tc.setting, e.rawEvents())
			}
		})
	}
}

func TestResetRestoresDefaultsAndKeepsContent(t *testing.T) {
	e := newEnv(t)
	im := buildImage("layer")
	e.pushImage(testRepo, "v1", im)
	e.set(map[string]any{settingAuthRequired: true})
	if e.do(http.MethodGet, "/v2/", nil).status != http.StatusUnauthorized {
		t.Fatal("auth_required did not take effect")
	}
	if res := e.control(http.MethodPost, "/scopes/"+e.runID+"/reset", nil); res.status != http.StatusNoContent {
		t.Fatalf("reset: %d", res.status)
	}
	if res := e.do(http.MethodGet, "/v2/"+testRepo+"/manifests/v1", nil); res.status != http.StatusOK {
		t.Errorf("after reset the pushed image is gone or still guarded: %d", res.status)
	}
}

func TestTwoScopesShareNothing(t *testing.T) {
	a := newEnv(t)
	b := newEnv(t)
	im := buildImage("layer")
	a.pushImage(testRepo, "v1", im)
	if res := b.do(http.MethodGet, "/v2/"+testRepo+"/manifests/v1", nil); res.errCode() != "NAME_UNKNOWN" {
		t.Errorf("run B sees run A's repository: %d %s", res.status, res.errCode())
	}
}
