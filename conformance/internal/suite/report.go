package suite

import (
	"fmt"
	"io"
	"strings"
)

type verdict int

const (
	verdictPass verdict = iota
	verdictFail
	verdictSkip
)

// outcome is what one check answers.
type outcome struct {
	verdict verdict
	msg     string
}

func pass(format string, a ...any) outcome {
	return outcome{verdictPass, fmt.Sprintf(format, a...)}
}

func fail(format string, a ...any) outcome {
	return outcome{verdictFail, fmt.Sprintf(format, a...)}
}

// notApplicable is a measured answer from the mock's declared shape, never a failure.
func notApplicable(format string, a ...any) outcome {
	return outcome{verdictSkip, "not applicable: " + fmt.Sprintf(format, a...)}
}

// report prints one line per check and remembers each verdict for prerequisites.
type report struct {
	out      io.Writer
	verdicts map[string]verdict
	counts   [3]int
	remarks  []string
}

func newReport(out io.Writer) *report {
	return &report{out: out, verdicts: map[string]verdict{}}
}

func (r *report) record(id string, o outcome) {
	r.verdicts[id] = o.verdict
	r.counts[o.verdict]++
	label := [...]string{"PASS", "FAIL", "SKIP"}[o.verdict]
	fmt.Fprintf(r.out, "%s %s — %s\n", label, id, oneLine(o.msg))
}

func (r *report) remark(s string) { r.remarks = append(r.remarks, oneLine(s)) }

// prerequisiteMet reports whether check id passed. A scoped id with no exact entry is
// met when every per-protocol instance of it passed.
func (r *report) prerequisiteMet(id string) (met bool, culprit string) {
	if v, ok := r.verdicts[id]; ok {
		return v == verdictPass, id
	}
	found := false
	for k, v := range r.verdicts {
		if strings.HasPrefix(k, id+"/") {
			found = true
			if v != verdictPass {
				return false, k
			}
		}
	}
	return found, id
}

func (r *report) failed() bool { return r.counts[verdictFail] > 0 }

func (r *report) finish(code int) {
	for _, s := range r.remarks {
		fmt.Fprintf(r.out, "remark: %s\n", s)
	}
	fmt.Fprintf(r.out, "remark: a SKIP proved nothing about the mock; read its reason before reading the exit code as acceptance.\n")
	fmt.Fprintf(r.out, "SUMMARY pass=%d fail=%d skip=%d exit=%d\n", r.counts[verdictPass], r.counts[verdictFail], r.counts[verdictSkip], code)
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
