package s3mock

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	s3Namespace   = "http://s3.amazonaws.com/doc/2006-03-01/"
	defaultMaxKey = 1000
	s3TimeLayout  = "2006-01-02T15:04:05.000Z"
)

// s3Error is S3's XML error body.
type s3Error struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string
	Message   string
	Resource  string `xml:",omitempty"`
	RequestID string `xml:"RequestId"`
}

func writeXML(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("x-amz-request-id", randomHex(8))
	w.Header().Set("Server", mockName)
	w.WriteHeader(status)
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(v) // the client hanging up mid-write is not the mock's problem
}

func writeS3Error(w http.ResponseWriter, req *request, status int, code, message, resource string) {
	if req.r.Method == http.MethodHead {
		w.Header().Set("x-amz-request-id", randomHex(8))
		w.WriteHeader(status) // a HEAD answer has no body
		return
	}
	if resource == "" {
		resource = req.r.URL.Path
	}
	writeXML(w, status, s3Error{Code: code, Message: message, Resource: resource, RequestID: randomHex(8)})
}

func writeStoreError(w http.ResponseWriter, req *request, e *storeError) {
	writeS3Error(w, req, e.status, e.code, e.message, e.resource)
}

// dispatch runs one request: faults, authentication, then the operation. It returns
// the setting that injected a fault, or "".
func (s *scope) dispatch(w http.ResponseWriter, req *request) string {
	if f := s.injectFault(w, req); f != "" {
		return f
	}
	if !s.authenticate(w, req) {
		return ""
	}
	switch req.operation {
	case "ListBuckets":
		s.listBuckets(w)
	case "CreateBucket":
		if e := s.store.createBucket(req.bucket, s.m.cfg.Now()); e != nil {
			writeStoreError(w, req, e)
			return ""
		}
		w.Header().Set("Location", "/"+req.bucket)
		w.WriteHeader(http.StatusOK)
	case "DeleteBucket":
		if e := s.store.deleteBucket(req.bucket); e != nil {
			writeStoreError(w, req, e)
			return ""
		}
		w.WriteHeader(http.StatusNoContent)
	case "HeadBucket":
		if !s.store.hasBucket(req.bucket) {
			writeStoreError(w, req, errBucketMissing(req.bucket))
			return ""
		}
		w.WriteHeader(http.StatusOK)
	case "ListObjects", "ListObjectsV2":
		s.listObjects(w, req)
	case "PutObject":
		s.putObject(w, req)
	case "GetObject", "HeadObject":
		return s.getObject(w, req)
	case "DeleteObject":
		if e := s.store.deleteObject(req.bucket, req.key); e != nil {
			writeStoreError(w, req, e)
			return ""
		}
		w.WriteHeader(http.StatusNoContent)
	case "CreateMultipartUpload":
		s.createUpload(w, req)
	case "UploadPart":
		s.uploadPart(w, req)
	case "CompleteMultipartUpload":
		s.completeUpload(w, req)
	case "AbortMultipartUpload":
		if e := s.store.abortUpload(req.bucket, req.key, req.r.URL.Query().Get("uploadId")); e != nil {
			writeStoreError(w, req, e)
			return ""
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeS3Error(w, req, 501, "NotImplemented", "A header you provided implies functionality that is not implemented, or the operation is not one this mock serves.", "")
	}
	return ""
}

var authErrors = map[error]struct {
	status  int
	message string
}{
	errAuthMalformed:     {400, "The authorization header is malformed; the credential scope, SignedHeaders or Signature is missing or invalid."},
	errQueryMalformed:    {400, "Query-string authentication requires the X-Amz-Algorithm, X-Amz-Credential, X-Amz-Signature, X-Amz-Date, X-Amz-SignedHeaders, and X-Amz-Expires parameters."},
	errAccessKeyUnknown:  {403, "The AWS Access Key Id you provided does not exist in our records."},
	errSignatureMismatch: {403, "The request signature we calculated does not match the signature you provided. Check your key and signing method."},
	errExpired:           {403, "Request has expired"},
	errSkewed:            {403, "The difference between the request time and the current time is too large."},
}

// authenticate verifies SigV4 against the scope's credentials; it answers the
// refusal itself and returns false when the request may not proceed.
func (s *scope) authenticate(w http.ResponseWriter, req *request) bool {
	r := req.r
	sr := sigRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Headers: lowerHeaders(r), Host: r.Host}
	a, signed, err := parseAuth(sr)
	if !signed {
		writeS3Error(w, req, 403, "AccessDenied", "Access Denied: the request is not signed.", "")
		return false
	}
	if err == nil {
		sr.PayloadHash = sr.Headers["x-amz-content-sha256"]
		if a.Presigned && sr.PayloadHash == "" {
			sr.PayloadHash = unsignedBody
		}
		if sr.PayloadHash == "" {
			writeS3Error(w, req, 400, "InvalidRequest", "Missing required header for this request: x-amz-content-sha256", "")
			return false
		}
		err = verify(sr, a, s.str(SettingAccessKey), s.str(SettingSecretKey), s.m.cfg.Now())
	}
	if err != nil {
		info := authErrors[err]
		code := err.Error()
		if err == errExpired || err == errSkewed {
			code = "AccessDenied"
			if err == errSkewed {
				code = "RequestTimeTooSkewed"
			}
		}
		writeS3Error(w, req, info.status, code, info.message, "")
		return false
	}
	switch h := sr.PayloadHash; {
	case strings.HasPrefix(h, "STREAMING-"):
		writeS3Error(w, req, 501, "NotImplemented", "Chunked (aws-chunked) payload signing is not implemented by this mock; send the body with an x-amz-content-sha256 of the whole payload or UNSIGNED-PAYLOAD.", "")
		return false
	case h != unsignedBody && (!hexDigest.MatchString(h) || h != bodySHA(req.body)):
		writeS3Error(w, req, 400, "XAmzContentSHA256Mismatch", "The provided 'x-amz-content-sha256' header does not match what was computed.", "")
		return false
	}
	return true
}

type xmlBucket struct {
	Name         string
	CreationDate string
}

func (s *scope) listBuckets(w http.ResponseWriter) {
	type result struct {
		XMLName xml.Name `xml:"ListAllMyBucketsResult"`
		Xmlns   string   `xml:"xmlns,attr"`
		Owner   struct{ ID, DisplayName string }
		Buckets struct {
			Bucket []xmlBucket
		}
	}
	res := result{Xmlns: s3Namespace}
	res.Owner.ID, res.Owner.DisplayName = "example-owner", "example-owner"
	for _, b := range s.store.listBuckets() {
		res.Buckets.Bucket = append(res.Buckets.Bucket, xmlBucket{b.Name, b.Created.UTC().Format(s3TimeLayout)})
	}
	writeXML(w, http.StatusOK, res)
}

type xmlContents struct {
	Key          string
	LastModified string
	ETag         string
	Size         int
	StorageClass string
}

func (s *scope) listObjects(w http.ResponseWriter, req *request) {
	q := req.r.URL.Query()
	maxKeys := defaultMaxKey
	if v := q.Get("max-keys"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeS3Error(w, req, 400, "InvalidArgument", "Provided max-keys not an integer or within integer range", "")
			return
		}
		maxKeys = min(n, defaultMaxKey)
	}
	v2 := q.Get("list-type") == "2"
	after := q.Get("marker")
	if v2 {
		after = q.Get("start-after")
		if tok := q.Get("continuation-token"); tok != "" {
			raw, err := base64.RawURLEncoding.DecodeString(tok)
			if err != nil {
				writeS3Error(w, req, 400, "InvalidArgument", "The continuation token provided is incorrect", "")
				return
			}
			after = string(raw)
		}
	}
	l, e := s.store.list(req.bucket, q.Get("prefix"), q.Get("delimiter"), after, maxKeys)
	if e != nil {
		writeStoreError(w, req, e)
		return
	}
	type prefix struct{ Prefix string }
	type result struct {
		XMLName               xml.Name `xml:"ListBucketResult"`
		Xmlns                 string   `xml:"xmlns,attr"`
		Name                  string
		Prefix                string
		Delimiter             string `xml:",omitempty"`
		MaxKeys               int
		KeyCount              int    `xml:",omitempty"`
		Marker                string `xml:",omitempty"`
		StartAfter            string `xml:",omitempty"`
		ContinuationToken     string `xml:",omitempty"`
		NextContinuationToken string `xml:",omitempty"`
		NextMarker            string `xml:",omitempty"`
		IsTruncated           bool
		Contents              []xmlContents
		CommonPrefixes        []prefix
	}
	res := result{Xmlns: s3Namespace, Name: req.bucket, Prefix: q.Get("prefix"), Delimiter: q.Get("delimiter"),
		MaxKeys: maxKeys, IsTruncated: l.Truncated}
	for _, o := range l.Objects {
		res.Contents = append(res.Contents, xmlContents{o.Key, o.LastModified.UTC().Format(s3TimeLayout), o.ETag, o.Size, "STANDARD"})
	}
	for _, p := range l.CommonPrefixes {
		res.CommonPrefixes = append(res.CommonPrefixes, prefix{p})
	}
	next := ""
	if l.Truncated {
		next = base64.RawURLEncoding.EncodeToString([]byte(l.NextToken))
	}
	if v2 {
		res.KeyCount = len(l.Objects) + len(l.CommonPrefixes)
		res.StartAfter = q.Get("start-after")
		res.ContinuationToken = q.Get("continuation-token")
		res.NextContinuationToken = next
	} else {
		res.Marker = q.Get("marker")
		if l.Truncated {
			res.NextMarker = l.NextToken
		}
	}
	writeXML(w, http.StatusOK, res)
}

// checkMD5 verifies the Content-MD5 header against the body, answering BadDigest.
func checkMD5(w http.ResponseWriter, req *request) bool {
	h := req.r.Header.Get("Content-MD5")
	if h == "" {
		return true
	}
	want, err := base64.StdEncoding.DecodeString(h)
	if err != nil || len(want) != md5.Size {
		writeS3Error(w, req, 400, "InvalidDigest", "The Content-MD5 you specified was invalid.", "")
		return false
	}
	got := md5.Sum(req.body)
	if string(want) != string(got[:]) {
		writeS3Error(w, req, 400, "BadDigest", "The Content-MD5 you specified did not match what we received.", "")
		return false
	}
	return true
}

func (s *scope) putObject(w http.ResponseWriter, req *request) {
	if req.r.Header.Get("x-amz-copy-source") != "" {
		writeS3Error(w, req, 501, "NotImplemented", "CopyObject is not implemented by this mock.", "")
		return
	}
	if !checkMD5(w, req) {
		return
	}
	ct := req.r.Header.Get("Content-Type")
	if ct == "" {
		ct = "binary/octet-stream"
	}
	etag, e := s.store.putObject(req.bucket, req.key, ct, req.body, s.m.cfg.Now())
	if e != nil {
		writeStoreError(w, req, e)
		return
	}
	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
}

// parseRange reads one "bytes=" range against size; ok is false for a header that is
// not a byte range (S3 then serves the whole object), unsatisfiable for a range outside it.
func parseRange(h string, size int) (start, end int, ok, unsatisfiable bool) {
	spec, found := strings.CutPrefix(h, "bytes=")
	if !found || strings.Contains(spec, ",") {
		return 0, 0, false, false
	}
	a, b, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, false, false
	}
	switch {
	case a == "":
		n, err := strconv.Atoi(b)
		if err != nil || n < 0 {
			return 0, 0, false, false
		}
		if n == 0 || size == 0 {
			return 0, 0, true, true
		}
		return max(size-n, 0), size - 1, true, false
	default:
		st, err := strconv.Atoi(a)
		if err != nil || st < 0 {
			return 0, 0, false, false
		}
		en := size - 1
		if b != "" {
			e2, err := strconv.Atoi(b)
			if err != nil || e2 < st {
				return 0, 0, false, false
			}
			en = min(e2, size-1)
		}
		if st >= size {
			return 0, 0, true, true
		}
		return st, en, true, false
	}
}

func (s *scope) getObject(w http.ResponseWriter, req *request) string {
	if prefix := s.str(SettingMissingPrefix); prefix != "" && strings.HasPrefix(req.key, prefix) {
		s.announce(req, SettingMissingPrefix, "NoSuchKey for "+req.key)
		writeStoreError(w, req, &storeError{404, "NoSuchKey", "The specified key does not exist.", "/" + req.bucket + "/" + req.key})
		return SettingMissingPrefix
	}
	o, e := s.store.getObject(req.bucket, req.key)
	if e != nil {
		writeStoreError(w, req, e)
		return ""
	}
	fault := ""
	if ms := s.num(SettingGetLatency); ms > 0 && req.r.Method == http.MethodGet {
		s.announce(req, SettingGetLatency, fmt.Sprintf("read delayed %d ms", ms))
		time.Sleep(time.Duration(ms) * time.Millisecond)
		fault = SettingGetLatency
	}
	h := w.Header()
	h.Set("ETag", o.etag)
	h.Set("Content-Type", o.contentType)
	h.Set("Last-Modified", o.lastModified.UTC().Format(http.TimeFormat))
	h.Set("Accept-Ranges", "bytes")
	h.Set("x-amz-request-id", randomHex(8))
	data := o.data
	status := http.StatusOK
	if rh := req.r.Header.Get("Range"); rh != "" {
		st, en, ok, bad := parseRange(rh, len(o.data))
		switch {
		case bad:
			h.Del("ETag")
			writeS3Error(w, req, 416, "InvalidRange", "The requested range is not satisfiable", "")
			return fault
		case ok:
			data, status = o.data[st:en+1], http.StatusPartialContent
			h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", st, en, len(o.data)))
		}
	}
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	if req.r.Method == http.MethodGet {
		_, _ = w.Write(data) // the client hanging up mid-body is not the mock's problem
	}
	return fault
}

func (s *scope) createUpload(w http.ResponseWriter, req *request) {
	ct := req.r.Header.Get("Content-Type")
	if ct == "" {
		ct = "binary/octet-stream"
	}
	id, e := s.store.createUpload(req.bucket, req.key, ct, s.m.cfg.Now())
	if e != nil {
		writeStoreError(w, req, e)
		return
	}
	type result struct {
		XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
		Xmlns    string   `xml:"xmlns,attr"`
		Bucket   string
		Key      string
		UploadID string `xml:"UploadId"`
	}
	writeXML(w, http.StatusOK, result{Xmlns: s3Namespace, Bucket: req.bucket, Key: req.key, UploadID: id})
}

func (s *scope) uploadPart(w http.ResponseWriter, req *request) {
	q := req.r.URL.Query()
	n, err := strconv.Atoi(q.Get("partNumber"))
	if err != nil {
		writeS3Error(w, req, 400, "InvalidArgument", "Part number must be an integer between 1 and 10000, inclusive.", "")
		return
	}
	if !checkMD5(w, req) {
		return
	}
	etag, e := s.store.putPart(req.bucket, req.key, q.Get("uploadId"), n, req.body)
	if e != nil {
		writeStoreError(w, req, e)
		return
	}
	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
}

func (s *scope) completeUpload(w http.ResponseWriter, req *request) {
	var doc struct {
		Parts []struct {
			PartNumber int
			ETag       string
		} `xml:"Part"`
	}
	if err := xml.Unmarshal(req.body, &doc); err != nil {
		writeS3Error(w, req, 400, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema.", "")
		return
	}
	want := make([]completedPart, len(doc.Parts))
	for i, p := range doc.Parts {
		want[i] = completedPart{p.PartNumber, p.ETag}
	}
	etag, e := s.store.completeUpload(req.bucket, req.key, req.r.URL.Query().Get("uploadId"), want, s.num(SettingMinPartBytes), s.m.cfg.Now())
	if e != nil {
		writeStoreError(w, req, e)
		return
	}
	type result struct {
		XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
		Xmlns    string   `xml:"xmlns,attr"`
		Location string
		Bucket   string
		Key      string
		ETag     string
	}
	writeXML(w, http.StatusOK, result{Xmlns: s3Namespace, Location: "/" + req.bucket + "/" + req.key, Bucket: req.bucket, Key: req.key, ETag: etag})
}
