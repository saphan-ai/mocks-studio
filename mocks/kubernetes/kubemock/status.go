package kubemock

import (
	"fmt"
	"net/http"
	"strings"
)

// apiError is a failure the Kubernetes API reports as a Status object.
type apiError struct {
	code    int
	reason  string
	message string
	name    string
	group   string
	kind    string // the plural resource name, as the API reports it in details.kind
	causes  []statusCause
}

type statusCause struct {
	Type    string `json:"reason"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}

func (e *apiError) Error() string { return e.message }

// status renders the Status object of the API concepts page.
func (e *apiError) status() map[string]any {
	st := map[string]any{
		"kind": "Status", "apiVersion": "v1", "metadata": map[string]any{},
		"status": "Failure", "message": e.message, "reason": e.reason, "code": e.code,
	}
	details := map[string]any{}
	if e.name != "" {
		details["name"] = e.name
	}
	if e.group != "" {
		details["group"] = e.group
	}
	if e.kind != "" {
		details["kind"] = e.kind
	}
	if len(e.causes) > 0 {
		details["causes"] = e.causes
	}
	if len(details) > 0 {
		st["details"] = details
	}
	return st
}

func writeAPIError(w http.ResponseWriter, e *apiError) {
	writeJSON(w, e.code, e.status())
}

var reasonByCode = map[int]string{
	http.StatusBadRequest:            "BadRequest",
	http.StatusUnauthorized:          "Unauthorized",
	http.StatusForbidden:             "Forbidden",
	http.StatusNotFound:              "NotFound",
	http.StatusMethodNotAllowed:      "MethodNotAllowed",
	http.StatusNotAcceptable:         "NotAcceptable",
	http.StatusConflict:              "Conflict",
	http.StatusGone:                  "Expired",
	http.StatusRequestEntityTooLarge: "RequestEntityTooLarge",
	http.StatusUnsupportedMediaType:  "UnsupportedMediaType",
	http.StatusUnprocessableEntity:   "Invalid",
	http.StatusTooManyRequests:       "TooManyRequests",
	http.StatusInternalServerError:   "InternalError",
	http.StatusServiceUnavailable:    "ServiceUnavailable",
	http.StatusGatewayTimeout:        "Timeout",
}

// reasonFor is the Status reason the API uses for an HTTP status.
func reasonFor(code int) string {
	if r, ok := reasonByCode[code]; ok {
		return r
	}
	return strings.ReplaceAll(http.StatusText(code), " ", "")
}

func errBadRequest(format string, args ...any) *apiError {
	return &apiError{code: http.StatusBadRequest, reason: "BadRequest", message: fmt.Sprintf(format, args...)}
}

func errNotFound(d *resourceDef, name string) *apiError {
	return &apiError{code: http.StatusNotFound, reason: "NotFound", name: name, group: d.group, kind: d.plural,
		message: fmt.Sprintf("%s %q not found", d.qualified(), name)}
}

func errAlreadyExists(d *resourceDef, name string) *apiError {
	return &apiError{code: http.StatusConflict, reason: "AlreadyExists", name: name, group: d.group, kind: d.plural,
		message: fmt.Sprintf("%s %q already exists", d.qualified(), name)}
}

func errConflict(d *resourceDef, name string) *apiError {
	return &apiError{code: http.StatusConflict, reason: "Conflict", name: name, group: d.group, kind: d.plural,
		message: fmt.Sprintf("Operation cannot be fulfilled on %s %q: the object has been modified; please apply your changes to the latest version and try again", d.qualified(), name)}
}

func errGone(requested, oldest int64) *apiError {
	return &apiError{code: http.StatusGone, reason: "Expired",
		message: fmt.Sprintf("too old resource version: %d (%d)", requested, oldest)}
}

func errInvalid(d *resourceDef, name string, causes []statusCause) *apiError {
	parts := make([]string, len(causes))
	for i, c := range causes {
		parts[i] = c.Field + ": " + c.Message
	}
	return &apiError{code: http.StatusUnprocessableEntity, reason: "Invalid", name: name, group: d.group, kind: d.plural, causes: causes,
		message: fmt.Sprintf("%s %q is invalid: %s", d.kind, name, strings.Join(parts, ", "))}
}

func errForced(code int) *apiError {
	return &apiError{code: code, reason: reasonFor(code), message: fmt.Sprintf("the mock answers %d to every call because the setting %s says so; set it back to 0", code, SettingErrorStatus)}
}

func errThrottled() *apiError {
	return &apiError{code: http.StatusTooManyRequests, reason: "TooManyRequests",
		message: fmt.Sprintf("the mock answers 429 to every call because the setting %s is true; retry after %d second(s) or set it back to false", SettingThrottle, throttleRetrySecs)}
}

func errNoRoute(path string) *apiError {
	return &apiError{code: http.StatusNotFound, reason: "NotFound", message: "the server could not find the requested resource: " + path}
}

func errMethod(method, path string) *apiError {
	return &apiError{code: http.StatusMethodNotAllowed, reason: "MethodNotAllowed", message: fmt.Sprintf("the method %s is not supported on %s by this mock", method, path)}
}
