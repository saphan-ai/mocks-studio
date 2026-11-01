package ocimock

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"
)

// Operations, named as the events carry them.
const (
	opCheck          = "check"
	opToken          = "token"
	opCatalog        = "catalog"
	opTagsList       = "tags.list"
	opManifestGet    = "manifest.get"
	opManifestHead   = "manifest.head"
	opManifestPut    = "manifest.put"
	opManifestDelete = "manifest.delete"
	opBlobGet        = "blob.get"
	opBlobHead       = "blob.head"
	opBlobDelete     = "blob.delete"
	opUploadStart    = "upload.start"
	opUploadStatus   = "upload.status"
	opUploadPatch    = "upload.patch"
	opUploadPut      = "upload.put"
	opUploadCancel   = "upload.cancel"
	opUnknown        = "unknown"
	opMethod         = "method-not-allowed"
)

const maxNameLen = 255

var (
	nameComponent = `[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*`
	namePattern   = regexp.MustCompile(`^` + nameComponent + `(?:/` + nameComponent + `)*$`)
	tagPattern    = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)
	digestPattern = regexp.MustCompile(`^sha256:([0-9a-f]{64})$`)
)

// route is what a request path names.
type route struct {
	op   string
	name string // repository
	ref  string // tag or digest for a manifest, digest for a blob, session id for an upload
}

func validName(n string) bool { return len(n) <= maxNameLen && namePattern.MatchString(n) }

func parseDigest(d string) (hexPart string, ok bool) {
	m := digestPattern.FindStringSubmatch(d)
	if m == nil {
		return "", false
	}
	return m[1], true
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// parseRoute maps a method and path to an operation. Repository names contain slashes,
// so the keyword segments are read from the right.
func parseRoute(method, path string) route {
	switch path {
	case "/token":
		if method == http.MethodGet {
			return route{op: opToken}
		}
		return route{op: opMethod}
	case "/v2", "/v2/":
		if method == http.MethodGet || method == http.MethodHead {
			return route{op: opCheck}
		}
		return route{op: opMethod}
	case "/v2/_catalog":
		if method == http.MethodGet {
			return route{op: opCatalog}
		}
		return route{op: opMethod}
	}
	rest, ok := strings.CutPrefix(path, "/v2/")
	if !ok {
		return route{op: opUnknown}
	}
	segs := strings.Split(rest, "/")
	n := len(segs)
	join := func(k int) string { return strings.Join(segs[:k], "/") }
	byMethod := func(rt route, ops map[string]string) route {
		op, ok := ops[method]
		if !ok {
			rt.op = opMethod
			return rt
		}
		rt.op = op
		return rt
	}
	switch {
	case n >= 3 && segs[n-2] == "tags" && segs[n-1] == "list":
		return byMethod(route{name: join(n - 2)}, map[string]string{http.MethodGet: opTagsList})
	case n >= 3 && segs[n-2] == "manifests":
		return byMethod(route{name: join(n - 2), ref: segs[n-1]}, map[string]string{
			http.MethodGet: opManifestGet, http.MethodHead: opManifestHead,
			http.MethodPut: opManifestPut, http.MethodDelete: opManifestDelete})
	case n >= 3 && segs[n-2] == "blobs" && segs[n-1] == "uploads": // no trailing slash
		return byMethod(route{name: join(n - 2)}, map[string]string{http.MethodPost: opUploadStart})
	case n >= 4 && segs[n-3] == "blobs" && segs[n-2] == "uploads":
		if segs[n-1] == "" {
			return byMethod(route{name: join(n - 3)}, map[string]string{http.MethodPost: opUploadStart})
		}
		return byMethod(route{name: join(n - 3), ref: segs[n-1]}, map[string]string{
			http.MethodGet: opUploadStatus, http.MethodPatch: opUploadPatch,
			http.MethodPut: opUploadPut, http.MethodDelete: opUploadCancel})
	case n >= 3 && segs[n-2] == "blobs":
		return byMethod(route{name: join(n - 2), ref: segs[n-1]}, map[string]string{
			http.MethodGet: opBlobGet, http.MethodHead: opBlobHead, http.MethodDelete: opBlobDelete})
	}
	return route{op: opUnknown}
}
