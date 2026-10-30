package scimmock

import (
	"net/http"
	"sort"
	"strings"
)

// attr builds one attribute definition of RFC 7643 §7.
func attr(name, typ string, multi, required bool, mutability, returned string, sub ...any) map[string]any {
	a := map[string]any{
		"name": name, "type": typ, "multiValued": multi, "required": required, "caseExact": false,
		"mutability": mutability, "returned": returned, "uniqueness": "none",
	}
	if len(sub) > 0 {
		a["subAttributes"] = sub
	}
	return a
}

func valueSub() []any {
	return []any{
		attr("value", "string", false, false, "readWrite", "default"),
		attr("display", "string", false, false, "readWrite", "default"),
		attr("type", "string", false, false, "readWrite", "default"),
		attr("primary", "boolean", false, false, "readWrite", "default"),
	}
}

func userSchema() map[string]any {
	userName := attr("userName", "string", false, true, "readWrite", "default")
	userName["uniqueness"] = "server"
	ext := attr("externalId", "string", false, false, "readWrite", "default")
	ext["caseExact"] = true
	attrs := []any{
		userName, ext,
		attr("displayName", "string", false, false, "readWrite", "default"),
		attr("nickName", "string", false, false, "readWrite", "default"),
		attr("profileUrl", "reference", false, false, "readWrite", "default"),
		attr("title", "string", false, false, "readWrite", "default"),
		attr("userType", "string", false, false, "readWrite", "default"),
		attr("preferredLanguage", "string", false, false, "readWrite", "default"),
		attr("locale", "string", false, false, "readWrite", "default"),
		attr("timezone", "string", false, false, "readWrite", "default"),
		attr("active", "boolean", false, false, "readWrite", "default"),
		attr("password", "string", false, false, "writeOnly", "never"),
		attr("name", "complex", false, false, "readWrite", "default",
			attr("formatted", "string", false, false, "readWrite", "default"),
			attr("familyName", "string", false, false, "readWrite", "default"),
			attr("givenName", "string", false, false, "readWrite", "default")),
		attr("emails", "complex", true, false, "readWrite", "default", valueSub()...),
		attr("phoneNumbers", "complex", true, false, "readWrite", "default", valueSub()...),
		attr("addresses", "complex", true, false, "readWrite", "default", valueSub()...),
		attr("ims", "complex", true, false, "readWrite", "default", valueSub()...),
		attr("photos", "complex", true, false, "readWrite", "default", valueSub()...),
		attr("roles", "complex", true, false, "readWrite", "default", valueSub()...),
		attr("entitlements", "complex", true, false, "readWrite", "default", valueSub()...),
		attr("x509Certificates", "complex", true, false, "readWrite", "default", valueSub()...),
		attr("groups", "complex", true, false, "readOnly", "default", valueSub()...),
	}
	return map[string]any{"schemas": []any{"urn:ietf:params:scim:schemas:core:2.0:Schema"}, "id": userURN, "name": "User", "description": "User Account", "attributes": attrs}
}

func groupSchema() map[string]any {
	ext := attr("externalId", "string", false, false, "readWrite", "default")
	ext["caseExact"] = true
	attrs := []any{
		attr("displayName", "string", false, true, "readWrite", "default"),
		ext,
		attr("members", "complex", true, false, "readWrite", "default", valueSub()...),
	}
	return map[string]any{"schemas": []any{"urn:ietf:params:scim:schemas:core:2.0:Schema"}, "id": groupURN, "name": "Group", "description": "Group", "attributes": attrs}
}

func resourceTypes(base string) []map[string]any {
	mk := func(name, endpoint, schema string) map[string]any {
		return map[string]any{
			"schemas": []any{"urn:ietf:params:scim:schemas:core:2.0:ResourceType"},
			"id":      name, "name": name, "endpoint": endpoint, "schema": schema,
			"meta": map[string]any{"resourceType": "ResourceType", "location": base + "/ResourceTypes/" + name},
		}
	}
	return []map[string]any{mk("User", "/Users", userURN), mk("Group", "/Groups", groupURN)}
}

func serviceProviderConfig(base string) map[string]any {
	return map[string]any{
		"schemas":          []any{"urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"},
		"documentationUri": "https://www.rfc-editor.org/rfc/rfc7644",
		"patch":            map[string]any{"supported": true},
		"bulk":             map[string]any{"supported": false, "maxOperations": 0, "maxPayloadSize": 0},
		"filter":           map[string]any{"supported": true, "maxResults": maxResults},
		"changePassword":   map[string]any{"supported": true},
		"sort":             map[string]any{"supported": false},
		"etag":             map[string]any{"supported": true},
		"authenticationSchemes": []any{map[string]any{
			"type": "oauthbearertoken", "name": "OAuth Bearer Token",
			"description": "Any bearer token is accepted; it is never recorded.", "primary": true,
		}},
		"meta": map[string]any{"resourceType": "ServiceProviderConfig", "location": base + "/ServiceProviderConfig"},
	}
}

// discovery answers /ServiceProviderConfig, /Schemas and /ResourceTypes (RFC 7644 §4).
func discovery(segs []string, base string) *reply {
	switch segs[0] {
	case "ServiceProviderConfig":
		if len(segs) == 1 {
			return newReply(http.StatusOK, serviceProviderConfig(base))
		}
	case "Schemas":
		all := []map[string]any{groupSchema(), userSchema()}
		sort.Slice(all, func(i, j int) bool { return all[i]["id"].(string) < all[j]["id"].(string) })
		return listOrOne(segs, all, "id")
	case "ResourceTypes":
		return listOrOne(segs, resourceTypes(base), "id")
	}
	return errReply(newErr(http.StatusNotFound, "", "No such discovery endpoint exists and the request was refused; use /ServiceProviderConfig, /Schemas or /ResourceTypes."))
}

func listOrOne(segs []string, items []map[string]any, key string) *reply {
	if len(segs) == 1 {
		res := make([]any, len(items))
		for i, it := range items {
			res[i] = it
		}
		return newReply(http.StatusOK, map[string]any{"schemas": []any{listURN}, "totalResults": len(items), "startIndex": 1, "itemsPerPage": len(items), "Resources": res})
	}
	for _, it := range items {
		if strings.EqualFold(it[key].(string), segs[1]) {
			return newReply(http.StatusOK, it)
		}
	}
	return errReply(newErr(http.StatusNotFound, "", "No %s with id %q exists and the request was refused; list /%s to see the ids.", segs[0], segs[1], segs[0]))
}
