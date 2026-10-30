package engine

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	eventSchema     = "mocks.event/v1"
	eventTimeLayout = "2006-01-02T15:04:05.000Z"
	maxSummary      = 512
	subscriberQueue = 4096
)

// Correlation sources of an event (spec §3.1).
const (
	sourceTraceparent = "traceparent"
	sourceGenerated   = "generated"
)

// mockRef is the `mock` object of every event.
type mockRef struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Contract string `json:"contract"`
}

// Event is one mocks.event/v1 envelope.
type Event struct {
	Schema            string         `json:"schema"`
	EventID           string         `json:"event_id"`
	At                string         `json:"at"`
	RunID             string         `json:"run_id"`
	CorrelationID     string         `json:"correlation_id"`
	CorrelationSource string         `json:"correlation_source"`
	Mock              mockRef        `json:"mock"`
	Kind              string         `json:"kind"`
	Direction         string         `json:"direction"`
	Protocol          string         `json:"protocol"`
	Summary           string         `json:"summary"`
	Outcome           string         `json:"outcome,omitempty"`
	Fault             string         `json:"fault,omitempty"`
	LatencyMS         *int           `json:"latency_ms,omitempty"`
	Payload           map[string]any `json:"payload,omitempty"`
}

// redactedPayload is the payload of an event that exports nothing of the content.
func redactedPayload(content []byte) map[string]any {
	sum := sha256.Sum256(content)
	return map[string]any{"redacted": true, "digest": "sha256:" + hex.EncodeToString(sum[:]), "size": len(content)}
}

// clearPayload carries only fields the manifest declares safe.
func clearPayload(fields map[string]any) map[string]any {
	return map[string]any{"redacted": false, "fields": fields}
}

// hub fans events out to the stream subscribers.
type hub struct {
	mu      sync.Mutex
	subs    map[*subscriber]struct{}
	dropped atomic.Int64
}

type subscriber struct {
	run string // "" delivers every run
	ch  chan []byte
}

func newHub() *hub { return &hub{subs: map[*subscriber]struct{}{}} }

func (h *hub) subscribe(run string) *subscriber {
	s := &subscriber{run: run, ch: make(chan []byte, subscriberQueue)}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

func (h *hub) unsubscribe(s *subscriber) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
}

// publish never blocks on a slow reader: a reader whose queue is full loses the event, and the loss is counted.
func (h *hub) publish(run string, line []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		if s.run != "" && s.run != run {
			continue
		}
		select {
		case s.ch <- line:
		default:
			h.dropped.Add(1)
		}
	}
}

// ids makes event ids unique within the process and across restarts.
type ids struct {
	nonce string
	n     atomic.Uint64
}

func newIDs() *ids {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the OS entropy source failing is not recoverable
	}
	return &ids{nonce: hex.EncodeToString(b)}
}

func (i *ids) next() string { return fmt.Sprintf("evt-%s-%08d", i.nonce, i.n.Add(1)) }

// newTraceID returns a valid, non-zero W3C trace-id.
func newTraceID() string {
	b := make([]byte, 16)
	for {
		if _, err := rand.Read(b); err != nil {
			panic(err) // the OS entropy source failing is not recoverable
		}
		if s := hex.EncodeToString(b); s != "00000000000000000000000000000000" {
			return s
		}
	}
}

func stamp(t time.Time) string { return t.UTC().Format(eventTimeLayout) }

func encodeEvent(e Event) []byte {
	b, err := json.Marshal(e)
	if err != nil {
		panic(err) // an Event holds only strings, numbers and JSON-decoded values
	}
	return b
}
