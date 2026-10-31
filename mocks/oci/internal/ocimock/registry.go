package ocimock

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	mediaImageManifest  = "application/vnd.oci.image.manifest.v1+json"
	mediaImageIndex     = "application/vnd.oci.image.index.v1+json"
	mediaDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	mediaDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
)

var manifestTypes = map[string]bool{mediaImageManifest: true, mediaImageIndex: true, mediaDockerManifest: true, mediaDockerList: true}

func isIndexType(t string) bool { return t == mediaImageIndex || t == mediaDockerList }

func (s *scope) dispatch(rec *recorder, r *http.Request, rt route) {
	switch rt.op {
	case opToken:
		s.handleToken(rec, r)
		return
	case opUnknown:
		regErr(rec, http.StatusNotFound, "UNSUPPORTED", "The path is not part of the OCI Distribution API and the request was refused; use /v2/ paths.", nil)
		return
	case opMethod:
		regErr(rec, http.StatusMethodNotAllowed, "UNSUPPORTED", "The method is not supported for this path and the request was refused; use the method the specification lists for it.", nil)
		return
	}
	if wait := s.countHit(); wait > 0 {
		rec.fault = settingRateLimit
		rec.Header().Set("Retry-After", strconv.Itoa(wait))
		regErr(rec, http.StatusTooManyRequests, "TOOMANYREQUESTS", "The rate limit of this scope is exceeded and the request was refused; wait for the Retry-After seconds and retry.", nil)
		return
	}
	if s.authRequired() && !s.bearerOK(r) {
		s.challenge(rec, r, rt)
		return
	}
	if rt.name != "" && !validName(rt.name) {
		regErr(rec, http.StatusBadRequest, "NAME_INVALID", "The repository name does not match the specification's pattern and the request was refused; use lowercase path components.", nil)
		return
	}
	switch rt.op {
	case opCheck:
		rec.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		writeJSON(rec, http.StatusOK, map[string]any{})
	case opCatalog:
		s.handleCatalog(rec, r)
	case opTagsList:
		s.handleTags(rec, r, rt)
	case opManifestGet, opManifestHead:
		s.handleManifestGet(rec, r, rt)
	case opManifestPut:
		s.handleManifestPut(rec, r, rt)
	case opManifestDelete:
		s.handleManifestDelete(rec, rt)
	case opBlobGet, opBlobHead:
		s.handleBlobGet(rec, r, rt)
	case opBlobDelete:
		s.handleBlobDelete(rec, rt)
	case opUploadStart:
		s.handleUploadStart(rec, r, rt)
	case opUploadStatus, opUploadPatch, opUploadPut, opUploadCancel:
		s.handleUpload(rec, r, rt)
	}
}

func blobLocation(name, digest string) string { return "/v2/" + name + "/blobs/" + digest }

func nameUnknown(w http.ResponseWriter, name string) {
	regErr(w, http.StatusNotFound, "NAME_UNKNOWN", "The repository is not known to this registry and the request was refused; push to it first.", map[string]string{"name": name})
}

// paginate cuts a sorted list by the n and last query parameters and sets the Link header.
func paginate(w http.ResponseWriter, r *http.Request, path string, items []string) ([]string, bool) {
	sort.Strings(items)
	q := r.URL.Query()
	if last := q.Get("last"); last != "" {
		items = items[sort.SearchStrings(items, last):]
		if len(items) > 0 && items[0] == last {
			items = items[1:]
		}
	}
	if ns := q.Get("n"); ns != "" {
		n, err := strconv.Atoi(ns)
		if err != nil || n < 0 {
			regErr(w, http.StatusBadRequest, "PAGINATION_NUMBER_INVALID", "The n parameter is not a non-negative integer and the request was refused; send a whole number.", nil)
			return nil, false
		}
		if n < len(items) {
			if n > 0 {
				next := url.Values{"n": {ns}, "last": {items[n-1]}}
				w.Header().Set("Link", "<"+path+"?"+next.Encode()+`>; rel="next"`)
			}
			items = items[:n]
		}
	}
	return items, true
}

func (s *scope) handleCatalog(w http.ResponseWriter, r *http.Request) {
	s.store.mu.Lock()
	names := make([]string, 0, len(s.store.repos))
	for n := range s.store.repos {
		names = append(names, n)
	}
	s.store.mu.Unlock()
	page, ok := paginate(w, r, "/v2/_catalog", names)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repositories": nonNil(page)})
}

func (s *scope) handleTags(w http.ResponseWriter, r *http.Request, rt route) {
	s.store.mu.Lock()
	rp := s.store.repos[rt.name]
	var tags []string
	if rp != nil {
		for t := range rp.tags {
			tags = append(tags, t)
		}
	}
	s.store.mu.Unlock()
	if rp == nil {
		nameUnknown(w, rt.name)
		return
	}
	page, ok := paginate(w, r, "/v2/"+rt.name+"/tags/list", tags)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": rt.name, "tags": nonNil(page)})
}

func nonNil(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

// checkReference refuses a reference that is neither a tag nor a sha256 digest.
func checkReference(w http.ResponseWriter, ref string) bool {
	if strings.Contains(ref, ":") {
		if _, ok := parseDigest(ref); !ok {
			regErr(w, http.StatusBadRequest, "DIGEST_INVALID", "The reference looks like a digest but is not sha256:<64 hex> and the request was refused; send a sha256 digest or a tag.", nil)
			return false
		}
		return true
	}
	if !tagPattern.MatchString(ref) {
		regErr(w, http.StatusBadRequest, "TAG_INVALID", "The reference is not a valid tag and the request was refused; use letters, digits, '_', '.' and '-', at most 128 characters.", nil)
		return false
	}
	return true
}

func (s *scope) handleManifestGet(w http.ResponseWriter, r *http.Request, rt route) {
	if !checkReference(w, rt.ref) {
		return
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	rp := s.store.repos[rt.name]
	if rp == nil {
		nameUnknown(w, rt.name)
		return
	}
	digest := rt.ref
	if _, isDigest := parseDigest(rt.ref); !isDigest {
		digest = rp.tags[rt.ref]
	}
	mf := rp.manifests[digest]
	if mf == nil {
		regErr(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "The manifest is not in this repository and the request was refused; push it first.", map[string]string{"reference": rt.ref})
		return
	}
	w.Header().Set("Content-Type", mf.mediaType)
	w.Header().Set("Content-Length", strconv.Itoa(len(mf.body)))
	w.Header().Set("Docker-Content-Digest", digest)
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(mf.body) // the client hanging up is not the mock's problem
	}
}

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

type manifestDoc struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        *descriptor  `json:"config"`
	Layers        []descriptor `json:"layers"`
	Manifests     []descriptor `json:"manifests"`
}

func invalidManifest(w http.ResponseWriter, msg string) {
	regErr(w, http.StatusBadRequest, "MANIFEST_INVALID", msg, nil)
}

func (s *scope) handleManifestPut(w http.ResponseWriter, r *http.Request, rt route) {
	if !checkReference(w, rt.ref) {
		return
	}
	ctype, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
	ctype = strings.TrimSpace(ctype)
	if !manifestTypes[ctype] {
		regErr(w, http.StatusBadRequest, "MANIFEST_INVALID", "The Content-Type is not an image manifest or index media type and the manifest was refused; send an OCI or Docker v2 manifest media type.", map[string]string{"allowed": strings.Join(sortedTypes(), ", ")})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxManifestBytes))
	if err != nil {
		invalidManifest(w, "The manifest body could not be read within 4 MiB and the manifest was refused; send a smaller document.")
		return
	}
	digest := digestOf(body)
	if _, isDigest := parseDigest(rt.ref); isDigest && rt.ref != digest {
		regErr(w, http.StatusBadRequest, "DIGEST_INVALID", "The manifest bytes do not hash to the digest in the URL and the manifest was refused; send the digest of the bytes you upload.", map[string]string{"expected": rt.ref, "computed": digest})
		return
	}
	var doc manifestDoc
	if err := json.Unmarshal(body, &doc); err != nil || doc.SchemaVersion != 2 {
		invalidManifest(w, "The body is not a JSON manifest with schemaVersion 2 and the manifest was refused; send a valid image manifest or index.")
		return
	}
	if doc.MediaType != "" && doc.MediaType != ctype {
		invalidManifest(w, "The mediaType inside the manifest differs from its Content-Type and the manifest was refused; make them equal.")
		return
	}
	if (isIndexType(ctype) && doc.Manifests == nil) || (!isIndexType(ctype) && doc.Config == nil) {
		invalidManifest(w, "The manifest lacks the members its media type requires (config for an image manifest, manifests for an index) and was refused; add them.")
		return
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	rp := s.store.ensureRepo(rt.name)
	if missing, bad := checkReferenced(rp, doc, ctype); bad != "" {
		invalidManifest(w, bad)
		return
	} else if missing != "" {
		regErr(w, http.StatusBadRequest, "MANIFEST_BLOB_UNKNOWN", "The manifest references a blob or manifest this repository does not hold and was refused; push it first.", map[string]string{"digest": missing})
		return
	}
	rp.manifests[digest] = &manifest{mediaType: ctype, body: body}
	if _, isDigest := parseDigest(rt.ref); !isDigest {
		rp.tags[rt.ref] = digest
	}
	w.Header().Set("Location", "/v2/"+rt.name+"/manifests/"+digest)
	w.Header().Set("Docker-Content-Digest", digest)
	w.WriteHeader(http.StatusCreated)
}

// checkReferenced finds the first referenced digest the repository lacks (missing) or a
// descriptor that is itself malformed (bad).
func checkReferenced(rp *repo, doc manifestDoc, ctype string) (missing, bad string) {
	var blobs, manifests []descriptor
	if isIndexType(ctype) {
		manifests = doc.Manifests
	} else {
		blobs = append(blobs, *doc.Config)
		blobs = append(blobs, doc.Layers...)
	}
	for _, d := range append(append([]descriptor{}, blobs...), manifests...) {
		if _, ok := parseDigest(d.Digest); !ok {
			return "", "A descriptor in the manifest carries a digest that is not sha256:<64 hex> and the manifest was refused; fix the descriptor."
		}
	}
	for _, d := range blobs {
		if !rp.blobs[d.Digest] {
			return d.Digest, ""
		}
	}
	for _, d := range manifests {
		if rp.manifests[d.Digest] == nil {
			return d.Digest, ""
		}
	}
	return "", ""
}

func sortedTypes() []string {
	out := make([]string, 0, len(manifestTypes))
	for t := range manifestTypes {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func (s *scope) handleManifestDelete(w http.ResponseWriter, rt route) {
	if !checkReference(w, rt.ref) {
		return
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	rp := s.store.repos[rt.name]
	if rp == nil {
		nameUnknown(w, rt.name)
		return
	}
	if _, isDigest := parseDigest(rt.ref); isDigest {
		if rp.manifests[rt.ref] == nil {
			regErr(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "The manifest is not in this repository and the delete was refused; check the digest.", map[string]string{"reference": rt.ref})
			return
		}
		delete(rp.manifests, rt.ref)
		for t, d := range rp.tags {
			if d == rt.ref {
				delete(rp.tags, t)
			}
		}
	} else {
		if _, ok := rp.tags[rt.ref]; !ok {
			regErr(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "The tag is not in this repository and the delete was refused; check the tag.", map[string]string{"reference": rt.ref})
			return
		}
		delete(rp.tags, rt.ref)
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *scope) handleBlobGet(w http.ResponseWriter, r *http.Request, rt route) {
	if _, ok := parseDigest(rt.ref); !ok {
		regErr(w, http.StatusBadRequest, "DIGEST_INVALID", "The blob digest is not sha256:<64 hex> and the request was refused; send a sha256 digest.", nil)
		return
	}
	if s.missingBlobs()[rt.ref] {
		w.(*recorder).fault = settingMissingBlob
		regErr(w, http.StatusNotFound, "BLOB_UNKNOWN", "The blob is configured as missing in this scope and the request was refused; clear the missing_blob setting to serve it.", map[string]string{"digest": rt.ref})
		return
	}
	s.store.mu.Lock()
	rp := s.store.repos[rt.name]
	if rp == nil {
		s.store.mu.Unlock()
		nameUnknown(w, rt.name)
		return
	}
	data, held := s.store.blobs[rt.ref], rp.blobs[rt.ref]
	s.store.mu.Unlock()
	if !held {
		regErr(w, http.StatusNotFound, "BLOB_UNKNOWN", "The blob is not in this repository and the request was refused; push it first.", map[string]string{"digest": rt.ref})
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Docker-Content-Digest", rt.ref)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
}

func (s *scope) handleBlobDelete(w http.ResponseWriter, rt route) {
	if _, ok := parseDigest(rt.ref); !ok {
		regErr(w, http.StatusBadRequest, "DIGEST_INVALID", "The blob digest is not sha256:<64 hex> and the request was refused; send a sha256 digest.", nil)
		return
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	rp := s.store.repos[rt.name]
	if rp == nil {
		nameUnknown(w, rt.name)
		return
	}
	if !rp.blobs[rt.ref] {
		regErr(w, http.StatusNotFound, "BLOB_UNKNOWN", "The blob is not in this repository and the delete was refused; check the digest.", map[string]string{"digest": rt.ref})
		return
	}
	delete(rp.blobs, rt.ref)
	w.WriteHeader(http.StatusAccepted)
}

// verifyBlob is the one place a blob's bytes are checked against the digest the client
// claimed; every upload path goes through it.
func verifyBlob(w http.ResponseWriter, claimed string, data []byte) bool {
	if _, ok := parseDigest(claimed); !ok {
		regErr(w, http.StatusBadRequest, "DIGEST_INVALID", "The digest parameter is missing or not sha256:<64 hex> and the upload was refused; send the sha256 digest of the blob.", nil)
		return false
	}
	if got := digestOf(data); got != claimed {
		regErr(w, http.StatusBadRequest, "DIGEST_INVALID", "The uploaded bytes do not hash to the claimed digest and the upload was refused; send the digest of the bytes you upload.", map[string]string{"expected": claimed, "computed": got})
		return false
	}
	return true
}

func readBlobBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBlobBytes))
	if err != nil {
		regErr(w, http.StatusRequestEntityTooLarge, "SIZE_INVALID", "The upload body could not be read within 64 MiB and the upload was refused; send smaller blobs.", nil)
		return nil, false
	}
	return data, true
}

func (s *scope) handleUploadStart(w http.ResponseWriter, r *http.Request, rt route) {
	q := r.URL.Query()
	if mount, from := q.Get("mount"), q.Get("from"); mount != "" && from != "" {
		s.store.mu.Lock()
		src := s.store.repos[from]
		if _, ok := parseDigest(mount); ok && src != nil && src.blobs[mount] {
			s.store.ensureRepo(rt.name).blobs[mount] = true
			s.store.mu.Unlock()
			w.Header().Set("Location", blobLocation(rt.name, mount))
			w.Header().Set("Docker-Content-Digest", mount)
			w.WriteHeader(http.StatusCreated)
			return
		}
		s.store.mu.Unlock() // not mountable: fall through to a regular session, as the specification says
	}
	if claimed := q.Get("digest"); claimed != "" { // monolithic upload
		data, ok := readBlobBody(w, r)
		if !ok || !verifyBlob(w, claimed, data) {
			return
		}
		s.store.mu.Lock()
		s.store.putBlob(rt.name, claimed, data)
		s.store.mu.Unlock()
		w.Header().Set("Location", blobLocation(rt.name, claimed))
		w.Header().Set("Docker-Content-Digest", claimed)
		w.WriteHeader(http.StatusCreated)
		return
	}
	id := randomHex(16)
	s.store.mu.Lock()
	s.store.ensureRepo(rt.name)
	s.store.uploads[id] = &upload{repo: rt.name}
	s.store.mu.Unlock()
	uploadHeaders(w, rt.name, id, 0)
	w.WriteHeader(http.StatusAccepted)
}

func uploadHeaders(w http.ResponseWriter, name, id string, size int) {
	w.Header().Set("Location", "/v2/"+name+"/blobs/uploads/"+id)
	w.Header().Set("Docker-Upload-UUID", id)
	w.Header().Set("Range", "0-"+strconv.Itoa(max(size-1, 0)))
	w.Header().Set("Content-Length", "0")
}

func (s *scope) handleUpload(w http.ResponseWriter, r *http.Request, rt route) {
	s.store.mu.Lock()
	up := s.store.uploads[rt.ref]
	if up == nil || up.repo != rt.name {
		s.store.mu.Unlock()
		regErr(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "The upload session is not open in this repository and the request was refused; start a new upload.", map[string]string{"session": clip(rt.ref)})
		return
	}
	s.store.mu.Unlock()
	switch rt.op {
	case opUploadStatus:
		s.store.mu.Lock()
		size := len(up.data)
		s.store.mu.Unlock()
		uploadHeaders(w, rt.name, rt.ref, size)
		w.WriteHeader(http.StatusNoContent)
	case opUploadCancel:
		s.store.mu.Lock()
		delete(s.store.uploads, rt.ref)
		s.store.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case opUploadPatch:
		s.uploadChunk(w, r, rt, up, false)
	case opUploadPut:
		s.uploadChunk(w, r, rt, up, true)
	}
}

// uploadChunk appends the request body to the session; with final it also verifies the
// digest and turns the session into a blob.
func (s *scope) uploadChunk(w http.ResponseWriter, r *http.Request, rt route, up *upload, final bool) {
	chunk, ok := readBlobBody(w, r)
	if !ok {
		return
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if cr := r.Header.Get("Content-Range"); cr != "" {
		var from, to int
		if _, err := fmt.Sscanf(cr, "%d-%d", &from, &to); err != nil || from != len(up.data) {
			w.Header().Set("Range", "0-"+strconv.Itoa(max(len(up.data)-1, 0)))
			regErr(w, http.StatusRequestedRangeNotSatisfiable, "BLOB_UPLOAD_INVALID", "The Content-Range does not continue where the session stopped and the chunk was refused; resume at the offset in the Range header.", nil)
			return
		}
	}
	if len(up.data)+len(chunk) > maxBlobBytes {
		regErr(w, http.StatusRequestEntityTooLarge, "SIZE_INVALID", "The session would exceed 64 MiB and the chunk was refused; send smaller blobs.", nil)
		return
	}
	up.data = append(up.data, chunk...)
	if !final {
		uploadHeaders(w, rt.name, rt.ref, len(up.data))
		w.WriteHeader(http.StatusAccepted)
		return
	}
	claimed := r.URL.Query().Get("digest")
	if !verifyBlob(w, claimed, up.data) {
		delete(s.store.uploads, rt.ref)
		return
	}
	s.store.putBlob(rt.name, claimed, up.data)
	delete(s.store.uploads, rt.ref)
	w.Header().Set("Location", blobLocation(rt.name, claimed))
	w.Header().Set("Docker-Content-Digest", claimed)
	w.WriteHeader(http.StatusCreated)
}
