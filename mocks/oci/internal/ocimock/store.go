package ocimock

import "sync"

// Limits that keep a runaway client from filling the host's memory.
const (
	maxManifestBytes = 4 << 20
	maxBlobBytes     = 64 << 20
)

type manifest struct {
	mediaType string
	body      []byte
}

// repo is one repository: the blobs it links, its manifests by digest and its tags.
type repo struct {
	blobs     map[string]bool
	manifests map[string]*manifest
	tags      map[string]string // tag -> manifest digest
}

type upload struct {
	repo string
	data []byte
}

// store is a scope's registry content. Blob bytes are kept once by digest; a repository
// holds links to them. mu guards every field, and a handler holds it for one operation.
type store struct {
	mu      sync.Mutex
	blobs   map[string][]byte
	repos   map[string]*repo
	uploads map[string]*upload
}

func newStore() *store {
	return &store{blobs: map[string][]byte{}, repos: map[string]*repo{}, uploads: map[string]*upload{}}
}

// ensureRepo returns the named repository, creating it on first write.
func (st *store) ensureRepo(name string) *repo {
	r := st.repos[name]
	if r == nil {
		r = &repo{blobs: map[string]bool{}, manifests: map[string]*manifest{}, tags: map[string]string{}}
		st.repos[name] = r
	}
	return r
}

func (st *store) putBlob(name, digest string, data []byte) {
	st.blobs[digest] = data
	st.ensureRepo(name).blobs[digest] = true
}
