package suite

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// sseMaxLine bounds one server-sent line; a longer line ends the stream.
const sseMaxLine = 1 << 20

// Event is one object received on the events stream.
type Event struct {
	Raw  map[string]any // nil when the data was not a JSON object
	Line string         // the data exactly as received
}

func (ev Event) str(key string) string {
	s, _ := ev.Raw[key].(string)
	return s
}

// eventStore collects the events of one stream connection, in arrival order.
type eventStore struct {
	mu      sync.Mutex
	events  []Event
	changed chan struct{}
	ended   error
}

func newEventStore() *eventStore { return &eventStore{changed: make(chan struct{})} }

func (s *eventStore) add(ev Event) {
	s.mu.Lock()
	s.events = append(s.events, ev)
	s.broadcastLocked()
	s.mu.Unlock()
}

func (s *eventStore) end(err error) {
	s.mu.Lock()
	s.ended = err
	s.broadcastLocked()
	s.mu.Unlock()
}

func (s *eventStore) broadcastLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// mark returns a position; since(mark) returns what arrived after it.
func (s *eventStore) mark() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

func (s *eventStore) since(mark int) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.events[mark:]...)
}

// waitFor blocks until pred holds over the events after mark, the stream ends, or the
// deadline passes. It returns those events and whether pred held.
func (s *eventStore) waitFor(mark int, wait time.Duration, pred func([]Event) bool) ([]Event, bool) {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		s.mu.Lock()
		got := append([]Event(nil), s.events[mark:]...)
		ch, ended := s.changed, s.ended
		s.mu.Unlock()
		if pred(got) {
			return got, true
		}
		if ended != nil {
			return got, false
		}
		select {
		case <-ch:
		case <-deadline.C:
			return got, false
		}
	}
}

// streamInfo is what opening the stream showed.
type streamInfo struct {
	status      int
	contentType string
}

// openStream connects to an events endpoint and collects events until ctx ends.
func openStream(ctx context.Context, url string, connectTimeout time.Duration) (*eventStore, streamInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, streamInfo{}, err
	}
	req.Header.Set("Accept", "text/event-stream")
	client := &http.Client{Transport: &http.Transport{DisableCompression: true, ResponseHeaderTimeout: connectTimeout}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, streamInfo{}, err
	}
	info := streamInfo{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type")}
	store := newEventStore()
	go func() {
		defer func() { _ = resp.Body.Close() }() // read side only; nothing to flush
		store.end(readSSE(resp, store))
	}()
	return store, info, nil
}

func readSSE(resp *http.Response, store *eventStore) error {
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), sseMaxLine)
	var data []string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if len(data) > 0 {
				store.add(parseData(strings.Join(data, "\n")))
				data = nil
			}
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return fmt.Errorf("event stream ended")
}

func parseData(data string) Event {
	ev := Event{Line: data}
	var raw map[string]any
	if err := json.Unmarshal([]byte(data), &raw); err == nil {
		ev.Raw = raw
	}
	return ev
}
