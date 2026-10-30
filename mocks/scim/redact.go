package scimmock

import (
	"encoding/json"
	"net/http"
	"strings"
)

// safeBodyFields maps the SCIM attributes of a request body that are exported in clear to the
// manifest's safe_fields names (a safe field name is lower-case, so userName becomes user_name).
// Every other attribute of a body — password above all — is left out of the event, and no
// header is ever read into one.
var safeBodyFields = map[string]string{"userName": "user_name", "displayName": "display_name", "active": "active"}

const maxSafeValue = 256

// requestPayload is the payload of a request.received event: the declared safe fields only.
func requestPayload(r *http.Request, body []byte) map[string]any {
	fields := map[string]any{"method": r.Method, "path": r.URL.Path}
	if seg := strings.Split(strings.Trim(r.URL.Path, "/"), "/"); resourceSegment(seg) >= 0 {
		fields["resource_type"] = seg[resourceSegment(seg)]
	}
	var obj map[string]any
	if json.Unmarshal(body, &obj) == nil {
		collectSafe(fields, obj)
		for _, raw := range asArr(obj["Operations"]) {
			op, _ := raw.(map[string]any)
			path, _ := op["path"].(string)
			if v, ok := op["value"]; ok && path == "" {
				if vo, ok := v.(map[string]any); ok {
					collectSafe(fields, vo)
				}
			} else if name, found := safeName(path); found {
				putSafe(fields, name, v)
			}
		}
	}
	return map[string]any{"redacted": false, "fields": fields}
}

func resourceSegment(seg []string) int {
	for i, s := range seg {
		switch s {
		case "Users", "Groups", "ServiceProviderConfig", "Schemas", "ResourceTypes":
			return i
		}
	}
	return -1
}

func safeName(attr string) (string, bool) {
	for k, field := range safeBodyFields {
		if strings.EqualFold(k, attr) {
			return field, true
		}
	}
	return "", false
}

func collectSafe(fields, obj map[string]any) {
	for k, v := range obj {
		if name, ok := safeName(k); ok {
			putSafe(fields, name, v)
		}
	}
}

func putSafe(fields map[string]any, name string, v any) {
	switch t := v.(type) {
	case string:
		if len(t) > maxSafeValue {
			t = t[:maxSafeValue]
		}
		fields[name] = t
	case bool:
		fields[name] = t
	}
}
