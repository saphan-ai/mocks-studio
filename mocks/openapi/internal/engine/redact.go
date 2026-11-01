package engine

import (
	"regexp"
	"sort"
	"strconv"
)

// RedactedMarker stands where a value was withheld from a log.
const RedactedMarker = "[redacted]"

// secretName matches the names of fields that carry a credential.
var secretName = regexp.MustCompile(`(?i)(pass(word|wd|phrase)?|pin|secret|token|api[-_]?key|credential|authorization|cookie|session|bearer|private[-_]?key)`)

// IsSecretName reports whether a header, parameter or body field is named like a credential.
func IsSecretName(name string) bool { return secretName.MatchString(name) }

// Redactor decides what a log line shows of one field's value. The engine writes every value it logs through it.
type Redactor func(name, value string) string

// NewRedactor returns the default redactor: a log carries the field's name and a marker, never its value. Only a field
// the operator declared safe, and that is not named like a credential, keeps its value.
func NewRedactor(safe []string) Redactor {
	allowed := make(map[string]bool, len(safe))
	for _, f := range safe {
		if !IsSecretName(f) {
			allowed[f] = true
		}
	}
	return func(name, value string) string {
		if allowed[name] {
			return value
		}
		return RedactedMarker
	}
}

// loggedField is a field the verbose log lists: where it is and its value, before redaction.
type loggedField struct{ Where, Name, Value string }

// render writes the fields as name=value pairs through the redactor, in a stable order.
func render(fields []loggedField, redact Redactor) string {
	sort.SliceStable(fields, func(i, j int) bool { return fields[i].Where < fields[j].Where })
	out := ""
	for _, f := range fields {
		if out != "" {
			out += " "
		}
		out += strconv.Quote(f.Where) + "=" + strconv.Quote(redact(f.Name, f.Value))
	}
	return out
}
