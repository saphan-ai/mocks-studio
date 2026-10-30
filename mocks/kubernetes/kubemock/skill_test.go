package kubemock

import (
	"bufio"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// skillSteps returns the lines of the fenced steps block of SKILL.md.
func skillSteps(t *testing.T, doc string) []string {
	t.Helper()
	var steps []string
	in := false
	sc := bufio.NewScanner(strings.NewReader(doc))
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
	for sc.Scan() {
		switch line := sc.Text(); {
		case line == "```steps":
			in = true
		case in && line == "```":
			return steps
		case in && strings.TrimSpace(line) != "":
			steps = append(steps, line)
		}
	}
	t.Fatal("SKILL.md has no ```steps block")
	return nil
}

func TestSkillFrontMatterNamesWhatAnAgentNeeds(t *testing.T) {
	t.Parallel()
	doc := string(readFileForTest(t, "../SKILL.md"))
	front, _, ok := strings.Cut(strings.TrimPrefix(doc, "---\n"), "\n---\n")
	if !strings.HasPrefix(doc, "---\n") || !ok {
		t.Fatal("SKILL.md does not start with a front matter block")
	}
	for _, want := range []string{"name: kubernetes-mock\n", "description: ", "license: Apache-2.0\n", "metadata:\n", "  saphan.origin: "} {
		if !strings.Contains(front+"\n", want) {
			t.Errorf("the front matter lacks %q", strings.TrimSpace(want))
		}
	}
}

func TestSkillWorkedExampleRuns(t *testing.T) {
	t.Parallel()
	steps := skillSteps(t, string(readFileForTest(t, "../SKILL.md")))
	r := newRig(t, 0)
	port := freePort(t)
	control, scopeURL, rv := r.m.ControlURL(), "http://127.0.0.1:"+strconv.Itoa(port), ""
	var cur *stream
	var sub *subscriber
	sub = r.m.subscribe("demo-run")
	defer r.m.unsubscribe(sub)
	sr := &scopeRig{rig: r, runID: "demo-run", sub: sub}
	subst := strings.NewReplacer("$CONTROL", control, "$SCOPE", scopeURL, "$PORT", strconv.Itoa(port))
	for _, line := range steps {
		verb, rest, _ := strings.Cut(line, " ")
		rest = subst.Replace(strings.ReplaceAll(rest, "$RV", rv))
		url, body, _ := strings.Cut(rest, " ")
		switch verb {
		case "POST", "PUT", "GET", "DELETE":
			req, err := http.NewRequest(verb, url, strings.NewReader(body))
			if err != nil {
				t.Fatalf("%s: %v", line, err)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s: %v", line, err)
			}
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close() // read in full above
			if resp.StatusCode < 200 || resp.StatusCode > 299 {
				t.Fatalf("%s answered %d: %s", line, resp.StatusCode, b)
			}
			if verb == "GET" {
				rv = str(path(reply{body: b}.json(t), "metadata", "resourceVersion"))
			}
		case "WATCH":
			cur = (&scopeRig{rig: r, base: ""}).watchURL(url)
		case "EXPECT":
			fields := strings.Fields(rest)
			if fields[0] == "END" {
				if ev, got, ended := cur.next(testWait); got || !ended {
					t.Fatalf("%s: the stream did not end (event %v)", line, ev)
				}
				continue
			}
			if ev := cur.mustNext(); ev.Type != fields[0] || ev.name() != fields[1] {
				t.Fatalf("%s: the stream delivered %s %q", line, ev.Type, ev.name())
			}
		case "EVENT":
			fields := strings.Fields(rest)
			if _, ok := sr.event(func(e map[string]any) bool { return e["kind"] == fields[0] && e["fault"] == fields[1] }); !ok {
				t.Fatalf("%s: no such event arrived", line)
			}
		default:
			t.Fatalf("unknown step verb in SKILL.md: %q", line)
		}
	}
}
