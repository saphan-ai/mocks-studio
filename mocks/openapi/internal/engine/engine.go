package engine

import (
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"sync"
	"time"

	"github.com/saphan-ai/mocks-studio/mocks/openapi/internal/oas"
)

const (
	defaultKeepAlive = 15 * time.Second
	shutdownWait     = 2 * time.Second
)

// Options configure an Engine beyond the manifest's own fields.
type Options struct {
	Config
	Log       io.Writer      // where the engine logs; os.Stderr when nil
	Verbose   bool           // also log the names of every header, query and body field, each with its redacted value
	Redact    Redactor       // what a log shows of a field's value; NewRedactor(SafeFields) when nil
	Rand      func() float64 // a source in [0,1) for fail_rate; math/rand/v2 when nil
	KeepAlive time.Duration  // idle comment interval of the event stream; defaultKeepAlive when zero
}

// Engine is one running mock: a manifest, a control surface and the scopes opened through it.
type Engine struct {
	doc        *oas.Document
	man        *Manifest
	manJSON    []byte
	opSettings map[string]string // operationId → setting name
	opts       Options
	log        *logger
	hub        *hub
	ids        *ids
	safe       map[string]bool

	mu     sync.Mutex
	scopes map[string]*scope
}

// New builds the engine for a loaded document.
func New(doc *oas.Document, opts Options) (*Engine, error) {
	man, names, err := BuildManifest(doc, opts.Config)
	if err != nil {
		return nil, err
	}
	if opts.Log == nil {
		opts.Log = os.Stderr
	}
	if opts.Redact == nil {
		opts.Redact = NewRedactor(opts.SafeFields)
	}
	if opts.Rand == nil {
		opts.Rand = rand.Float64
	}
	if opts.KeepAlive <= 0 {
		opts.KeepAlive = defaultKeepAlive
	}
	e := &Engine{
		doc: doc, man: man, manJSON: man.JSON(), opSettings: names, opts: opts,
		log: &logger{w: opts.Log}, hub: newHub(), ids: newIDs(),
		safe: map[string]bool{}, scopes: map[string]*scope{},
	}
	for _, f := range opts.SafeFields {
		e.safe[f] = true
	}
	return e, nil
}

// Manifest is the manifest this engine serves.
func (e *Engine) Manifest() *Manifest { return e.man }

// ManifestJSON is the exact bytes of the served manifest.
func (e *Engine) ManifestJSON() []byte { return e.manJSON }

// Close stops every scope's listener.
func (e *Engine) Close() {
	e.mu.Lock()
	open := make([]*scope, 0, len(e.scopes))
	for _, s := range e.scopes {
		open = append(open, s)
	}
	e.scopes = map[string]*scope{}
	e.mu.Unlock()
	for _, s := range open {
		s.stop()
	}
}

// logger writes engine log lines, one at a time.
type logger struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *logger) printf(format string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// a failing log writer is not something the mock can report anywhere else
	_, _ = fmt.Fprintf(l.w, "%s %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, a...))
}

// publish stamps an event with the mock's identity, delivers it to the stream and returns it.
func (e *Engine) publish(ev Event) {
	ev.Schema = eventSchema
	ev.EventID = e.ids.next()
	ev.At = stamp(time.Now())
	ev.Mock = mockRef{Name: e.man.Name, Version: e.man.Version, Contract: Contract}
	ev.Protocol = "http"
	if len(ev.Summary) > maxSummary {
		ev.Summary = ev.Summary[:maxSummary]
	}
	e.hub.publish(ev.RunID, encodeEvent(ev))
}

// listenerSpec is the listener the core assigns to a run.
type listenerSpec struct {
	Protocol string `json:"protocol"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
}

func (l listenerSpec) addr() string { return net.JoinHostPort(l.Host, fmt.Sprint(l.Port)) }
