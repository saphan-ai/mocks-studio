package scimmock

import (
	"net/http"
	"reflect"
	"sort"
	"strings"
)

const patchOpURN = "urn:ietf:params:scim:api:messages:2.0:PatchOp"

// patch applies an RFC 7644 §3.5.2 PatchOp to a resource: all operations or none.
func (s *scope) patch(r *resource, body map[string]any) *scimErr {
	var ops []any
	for k, v := range body {
		if strings.EqualFold(k, "Operations") {
			ops, _ = v.([]any)
		}
	}
	if len(ops) == 0 {
		return newErr(http.StatusBadRequest, "invalidSyntax", "The PATCH body has no Operations array and was refused; send {\"schemas\":[%q],\"Operations\":[{\"op\":\"replace\",\"path\":\"active\",\"value\":false}]}.", patchOpURN)
	}
	attrs := deepCopy(r.attrs).(map[string]any)
	for i, raw := range ops {
		op, ok := raw.(map[string]any)
		if !ok {
			return newErr(http.StatusBadRequest, "invalidSyntax", "Operation %d is not an object and the PATCH was refused; send {\"op\":...,\"path\":...,\"value\":...}.", i)
		}
		name, _ := op["op"].(string)
		name = strings.ToLower(name)
		if name != "add" && name != "replace" && name != "remove" {
			return newErr(http.StatusBadRequest, "invalidSyntax", "Operation %d has op %v, which is not add, replace or remove; the PATCH was refused.", i, op["op"])
		}
		path, _ := op["path"].(string)
		if err := applyOp(r.kind, attrs, name, path, op["value"]); err != nil {
			return err
		}
	}
	if err := checkTypes(r.kind, attrs); err != nil {
		return err
	}
	return s.commit(r, attrs)
}

type patchPath struct {
	attr   string
	filter filterNode
	sub    string
}

func parsePatchPath(path string) (patchPath, *scimErr) {
	for _, urn := range []string{userURN + ":", groupURN + ":"} {
		if len(path) > len(urn) && strings.EqualFold(path[:len(urn)], urn) {
			path = path[len(urn):]
		}
	}
	var pp patchPath
	if open := strings.IndexByte(path, '['); open >= 0 {
		closeAt := strings.LastIndexByte(path, ']')
		if closeAt < open {
			return pp, newErr(http.StatusBadRequest, "invalidPath", "The path %q has an unclosed bracket and the PATCH was refused; close it.", path)
		}
		f, err := parseFilter(path[open+1 : closeAt])
		if err != nil {
			return pp, newErr(http.StatusBadRequest, "invalidFilter", "The filter inside the path %q is not valid (%v) and the PATCH was refused; fix it.", path, err)
		}
		pp.attr, pp.filter = path[:open], f
		rest := path[closeAt+1:]
		if rest != "" {
			if !strings.HasPrefix(rest, ".") {
				return pp, newErr(http.StatusBadRequest, "invalidPath", "The path %q has text after the bracket that is not .subAttribute and the PATCH was refused.", path)
			}
			pp.sub = rest[1:]
		}
		return pp, nil
	}
	pp.attr, pp.sub, _ = strings.Cut(path, ".")
	return pp, nil
}

func applyOp(kind string, attrs map[string]any, op, path string, value any) *scimErr {
	if path == "" {
		if op == "remove" {
			return newErr(http.StatusBadRequest, "noTarget", "A remove operation has no path and the PATCH was refused; name the attribute to remove in path.")
		}
		obj, ok := value.(map[string]any)
		if !ok {
			return newErr(http.StatusBadRequest, "invalidValue", "An %s operation without a path needs an object value and the PATCH was refused; send {\"attribute\": value}.", op)
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := applyOp(kind, attrs, op, k, obj[k]); err != nil {
				return err
			}
		}
		return nil
	}
	pp, err := parsePatchPath(path)
	if err != nil {
		return err
	}
	if isReadOnly(kind, pp.attr) {
		return newErr(http.StatusBadRequest, "mutability", "The attribute %q is read-only and the PATCH was refused; leave it out.", pp.attr)
	}
	canon, typ, ok := canonAttr(kind, pp.attr)
	if !ok {
		return newErr(http.StatusBadRequest, "invalidPath", "The path %q names no attribute of a %s this mock models and the PATCH was refused; use a core %s attribute.", path, kind, kind)
	}
	if canon == "password" {
		return nil // accepted and discarded: the mock stores no credential
	}
	if remove := op == "remove"; remove && canon == requiredAttr(kind) && pp.sub == "" {
		return newErr(http.StatusBadRequest, "mutability", "The attribute %q is required and cannot be removed; the PATCH was refused, use replace instead.", canon)
	}
	switch typ {
	case aString, aBool:
		if pp.sub != "" || pp.filter != nil {
			return newErr(http.StatusBadRequest, "invalidPath", "The attribute %q is simple and has no sub-attributes or filter; the PATCH was refused.", canon)
		}
		if op == "remove" {
			delete(attrs, canon)
		} else {
			attrs[canon] = deepCopy(value)
		}
	case aComplex:
		return patchComplex(attrs, canon, pp, op, value)
	default:
		return patchMulti(attrs, canon, pp, op, value)
	}
	return nil
}

func patchComplex(attrs map[string]any, canon string, pp patchPath, op string, value any) *scimErr {
	cur, _ := attrs[canon].(map[string]any)
	if pp.filter != nil {
		return newErr(http.StatusBadRequest, "invalidPath", "The attribute %q is not multi-valued and takes no filter; the PATCH was refused.", canon)
	}
	if pp.sub == "" {
		switch op {
		case "remove":
			delete(attrs, canon)
		case "add":
			obj, isObj := value.(map[string]any)
			if cur != nil && isObj {
				for k, v := range obj {
					cur[k] = deepCopy(v)
				}
			} else {
				attrs[canon] = deepCopy(value)
			}
		default:
			attrs[canon] = deepCopy(value)
		}
		return nil
	}
	if op == "remove" {
		delete(cur, pp.sub)
		return nil
	}
	if cur == nil {
		cur = map[string]any{}
		attrs[canon] = cur
	}
	cur[pp.sub] = deepCopy(value)
	return nil
}

func patchMulti(attrs map[string]any, canon string, pp patchPath, op string, value any) *scimErr {
	arr := asArr(attrs[canon])
	defer func() {
		if a := asArr(attrs[canon]); len(a) == 0 {
			delete(attrs, canon)
		}
	}()
	switch {
	case pp.filter == nil && pp.sub == "":
		return patchMultiWhole(attrs, canon, arr, op, value)
	case pp.filter == nil: // attr.sub applies to every element
		for _, e := range arr {
			elemSet(e.(map[string]any), pp.sub, op, value)
		}
		if len(arr) == 0 && op != "remove" {
			attrs[canon] = []any{map[string]any{pp.sub: deepCopy(value)}}
		}
		return nil
	}
	matched := false
	kept := make([]any, 0, len(arr))
	for _, e := range arr {
		el := e.(map[string]any)
		if !pp.filter.eval(el) {
			kept = append(kept, e)
			continue
		}
		matched = true
		switch {
		case pp.sub != "":
			elemSet(el, pp.sub, op, value)
			kept = append(kept, el)
		case op == "remove":
			// dropped
		case op == "add":
			obj, ok := value.(map[string]any)
			if !ok {
				return newErr(http.StatusBadRequest, "invalidValue", "The value for a filtered add must be an object and the PATCH was refused; send an object.")
			}
			for k, v := range obj {
				el[k] = deepCopy(v)
			}
			kept = append(kept, el)
		default:
			obj, ok := value.(map[string]any)
			if !ok {
				return newErr(http.StatusBadRequest, "invalidValue", "The value for a filtered replace must be an object and the PATCH was refused; send an object.")
			}
			kept = append(kept, deepCopy(obj))
		}
	}
	if !matched {
		return newErr(http.StatusBadRequest, "noTarget", "No element of %q matches the path filter and the PATCH was refused; check the filter against the current resource.", canon)
	}
	attrs[canon] = kept
	return nil
}

func elemSet(el map[string]any, sub, op string, value any) {
	if op == "remove" {
		delete(el, sub)
		return
	}
	el[sub] = deepCopy(value)
}

func patchMultiWhole(attrs map[string]any, canon string, arr []any, op string, value any) *scimErr {
	if op == "remove" && value == nil {
		delete(attrs, canon)
		return nil
	}
	vals, ok := value.([]any)
	if !ok {
		if obj, isObj := value.(map[string]any); isObj && op != "replace" {
			vals, ok = []any{obj}, true
		}
	}
	if !ok {
		return newErr(http.StatusBadRequest, "invalidValue", "The value for %q must be an array of objects and the PATCH was refused; send an array.", canon)
	}
	switch op {
	case "replace":
		attrs[canon] = deepCopy(vals)
	case "add":
		for _, v := range vals {
			if !containsElem(arr, v) {
				arr = append(arr, deepCopy(v))
			}
		}
		attrs[canon] = arr
	default: // remove the named elements
		kept := make([]any, 0, len(arr))
		for _, e := range arr {
			if !containsElem(vals, e) {
				kept = append(kept, e)
			}
		}
		attrs[canon] = kept
	}
	return nil
}

// containsElem reports whether list holds an element equal to e, or — when both carry a
// "value" — naming the same value (so {"value":"id"} removes a member whose display was added).
func containsElem(list []any, e any) bool {
	em, _ := e.(map[string]any)
	for _, x := range list {
		if reflect.DeepEqual(x, e) {
			return true
		}
		xm, _ := x.(map[string]any)
		if xv, ok := xm["value"]; ok && em != nil {
			if ev, ok := em["value"]; ok && xv == ev {
				return true
			}
		}
	}
	return false
}
