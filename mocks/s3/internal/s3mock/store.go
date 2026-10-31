package s3mock

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	maxKeyBytes = 1024
	maxParts    = 10000
)

var bucketNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

type object struct {
	data         []byte
	etag         string // quoted
	contentType  string
	lastModified time.Time
}

type part struct {
	data []byte
	etag string
}

type upload struct {
	bucket, key string
	contentType string
	parts       map[int]part
	started     time.Time
}

type bucket struct {
	created time.Time
	objects map[string]*object
}

// store is one scope's buckets; two scopes never share one.
type store struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	uploads map[string]*upload
	seq     int
}

func newStore() *store {
	return &store{buckets: map[string]*bucket{}, uploads: map[string]*upload{}}
}

func md5Hex(b []byte) string {
	s := md5.Sum(b)
	return hex.EncodeToString(s[:])
}

// storeError carries the S3 error a failed store operation answers with.
type storeError struct {
	status   int
	code     string
	message  string
	resource string
}

func (e *storeError) Error() string { return e.code + ": " + e.message }

func errBucketMissing(name string) *storeError {
	return &storeError{404, "NoSuchBucket", "The specified bucket does not exist.", name}
}

func (s *store) createBucket(name string, now time.Time) *storeError {
	if !bucketNamePattern.MatchString(name) {
		return &storeError{400, "InvalidBucketName", "The specified bucket is not valid.", name}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.buckets[name]; ok {
		return &storeError{409, "BucketAlreadyOwnedByYou", "Your previous request to create the named bucket succeeded and you already own it.", name}
	}
	s.buckets[name] = &bucket{created: now, objects: map[string]*object{}}
	return nil
}

func (s *store) deleteBucket(name string) *storeError {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[name]
	if !ok {
		return errBucketMissing(name)
	}
	if len(b.objects) > 0 {
		return &storeError{409, "BucketNotEmpty", "The bucket you tried to delete is not empty.", name}
	}
	delete(s.buckets, name)
	return nil
}

func (s *store) hasBucket(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.buckets[name]
	return ok
}

type bucketInfo struct {
	Name    string
	Created time.Time
}

func (s *store) listBuckets() []bucketInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]bucketInfo, 0, len(s.buckets))
	for n, b := range s.buckets {
		out = append(out, bucketInfo{n, b.created})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *store) putObject(bkt, key, contentType string, data []byte, now time.Time) (string, *storeError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[bkt]
	if !ok {
		return "", errBucketMissing(bkt)
	}
	if len(key) > maxKeyBytes {
		return "", &storeError{400, "KeyTooLongError", "Your key is too long.", key}
	}
	etag := `"` + md5Hex(data) + `"`
	b.objects[key] = &object{data: append([]byte(nil), data...), etag: etag, contentType: contentType, lastModified: now}
	return etag, nil
}

func (s *store) getObject(bkt, key string) (*object, *storeError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[bkt]
	if !ok {
		return nil, errBucketMissing(bkt)
	}
	o, ok := b.objects[key]
	if !ok {
		return nil, &storeError{404, "NoSuchKey", "The specified key does not exist.", "/" + bkt + "/" + key}
	}
	return o, nil
}

func (s *store) deleteObject(bkt, key string) *storeError {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[bkt]
	if !ok {
		return errBucketMissing(bkt)
	}
	delete(b.objects, key) // S3 answers 204 for a key that is not there
	return nil
}

type listing struct {
	Objects        []listedObject
	CommonPrefixes []string
	Truncated      bool
	NextToken      string
}

type listedObject struct {
	Key          string
	ETag         string
	Size         int
	LastModified time.Time
}

// list returns keys after the token (a key), with prefix and delimiter applied;
// a common prefix counts as one entry toward maxKeys.
func (s *store) list(bkt, prefix, delimiter, after string, maxKeys int) (listing, *storeError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[bkt]
	if !ok {
		return listing{}, errBucketMissing(bkt)
	}
	keys := make([]string, 0, len(b.objects))
	for k := range b.objects {
		if strings.HasPrefix(k, prefix) && k > after {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var out listing
	seen := map[string]bool{}
	count := 0
	for _, k := range keys {
		entry := k
		if delimiter != "" {
			if i := strings.Index(k[len(prefix):], delimiter); i >= 0 {
				entry = k[:len(prefix)+i+len(delimiter)]
			}
		}
		if entry != k && seen[entry] {
			continue
		}
		if count == maxKeys {
			out.Truncated = true
			break
		}
		count++
		out.NextToken = k
		if entry != k {
			seen[entry] = true
			out.CommonPrefixes = append(out.CommonPrefixes, entry)
			out.NextToken = entry + "\U0010FFFF" // resume after every key under the prefix
			continue
		}
		o := b.objects[k]
		out.Objects = append(out.Objects, listedObject{k, o.etag, len(o.data), o.lastModified})
	}
	if !out.Truncated {
		out.NextToken = ""
	}
	return out, nil
}

func (s *store) createUpload(bkt, key, contentType string, now time.Time) (string, *storeError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.buckets[bkt]; !ok {
		return "", errBucketMissing(bkt)
	}
	s.seq++
	id := fmt.Sprintf("upl-%06d-%s", s.seq, randomHex(6))
	s.uploads[id] = &upload{bucket: bkt, key: key, contentType: contentType, parts: map[int]part{}, started: now}
	return id, nil
}

func (s *store) lookupUpload(bkt, key, id string) (*upload, *storeError) {
	u, ok := s.uploads[id]
	if !ok || u.bucket != bkt || u.key != key {
		return nil, &storeError{404, "NoSuchUpload", "The specified multipart upload does not exist. The upload ID might be invalid, or the multipart upload might have been aborted or completed.", id}
	}
	return u, nil
}

func (s *store) putPart(bkt, key, id string, n int, data []byte) (string, *storeError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, err := s.lookupUpload(bkt, key, id)
	if err != nil {
		return "", err
	}
	if n < 1 || n > maxParts {
		return "", &storeError{400, "InvalidArgument", "Part number must be an integer between 1 and 10000, inclusive.", ""}
	}
	etag := `"` + md5Hex(data) + `"`
	u.parts[n] = part{data: append([]byte(nil), data...), etag: etag}
	return etag, nil
}

type completedPart struct {
	Number int
	ETag   string
}

func (s *store) completeUpload(bkt, key, id string, want []completedPart, minPart int, now time.Time) (string, *storeError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, err := s.lookupUpload(bkt, key, id)
	if err != nil {
		return "", err
	}
	if len(want) == 0 {
		return "", &storeError{400, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema.", ""}
	}
	var body []byte
	var sums []byte
	for i, w := range want {
		p, ok := u.parts[w.Number]
		if !ok || strings.Trim(w.ETag, `"`) != strings.Trim(p.etag, `"`) {
			return "", &storeError{400, "InvalidPart", "One or more of the specified parts could not be found. The part might not have been uploaded, or the specified entity tag might not have matched the part's entity tag.", id}
		}
		if i > 0 && w.Number <= want[i-1].Number {
			return "", &storeError{400, "InvalidPartOrder", "The list of parts was not in ascending order. The parts list must be specified in order by part number.", id}
		}
		if i < len(want)-1 && len(p.data) < minPart {
			return "", &storeError{400, "EntityTooSmall", "Your proposed upload is smaller than the minimum allowed object size.", id}
		}
		body = append(body, p.data...)
		raw, _ := hex.DecodeString(strings.Trim(p.etag, `"`))
		sums = append(sums, raw...)
	}
	b, ok := s.buckets[bkt]
	if !ok {
		return "", errBucketMissing(bkt)
	}
	etag := fmt.Sprintf(`"%s-%d"`, md5Hex(sums), len(want))
	b.objects[key] = &object{data: body, etag: etag, contentType: u.contentType, lastModified: now}
	delete(s.uploads, id)
	return etag, nil
}

func (s *store) abortUpload(bkt, key, id string) *storeError {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.lookupUpload(bkt, key, id); err != nil {
		return err
	}
	delete(s.uploads, id)
	return nil
}
