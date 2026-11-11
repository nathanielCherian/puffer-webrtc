// Package logs writes one directory per session: meta.json plus one
// JSON-lines file per concern (events, rtcp, cc, encoder, snapshots, owd,
// client). Every record is {"t": <unix ms, µs resolution>, "kind", "data"}.
package logs

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Ms is t as unix milliseconds with microsecond resolution.
func Ms(t time.Time) float64 { return float64(t.UnixMicro()) / 1000 }

// NowMs is Ms(time.Now()).
func NowMs() float64 { return Ms(time.Now()) }

// flushInterval keeps files on disk fresh enough for the dashboard's live view.
const flushInterval = time.Second

type record struct {
	T    float64 `json:"t"`
	Kind string  `json:"kind"`
	Data any     `json:"data"`
}

type file struct {
	f *os.File
	w *bufio.Writer
}

// Session is one session's log directory. It is safe for concurrent use;
// writes after Close are dropped.
type Session struct {
	Dir string

	mu     sync.Mutex
	files  map[string]*file
	closed bool
	done   chan struct{}
}

// NewSession creates <root>/<UTC time>_<id>/.
func NewSession(root, id string) (*Session, error) {
	dir := filepath.Join(root, time.Now().UTC().Format("20060102T150405Z")+"_"+id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Session{Dir: dir, files: map[string]*file{}, done: make(chan struct{})}
	go s.flushLoop()
	return s, nil
}

func (s *Session) flushLoop() {
	t := time.NewTicker(flushInterval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			s.Flush()
		}
	}
}

// Log writes a record stamped now to <name>.jsonl.
func (s *Session) Log(name, kind string, data any) { s.LogAt(name, time.Now(), kind, data) }

// LogAt writes a record stamped t (the event time) to <name>.jsonl.
func (s *Session) LogAt(name string, t time.Time, kind string, data any) {
	s.Write(name, record{T: Ms(t), Kind: kind, Data: data})
}

// Write appends v as one JSON line to <name>.jsonl.
func (s *Session) Write(name string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("logs: %s/%s.jsonl: %v", s.Dir, name, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	f, ok := s.files[name]
	if !ok {
		fh, err := os.OpenFile(filepath.Join(s.Dir, name+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			log.Printf("logs: %v", err)
			return
		}
		f = &file{f: fh, w: bufio.NewWriterSize(fh, 64<<10)}
		s.files[name] = f
	}
	f.w.Write(b)
	f.w.WriteByte('\n')
}

// WriteMeta writes meta.json (indented), replacing any previous one.
func (s *Session) WriteMeta(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("meta.json: %w", err)
	}
	return os.WriteFile(filepath.Join(s.Dir, "meta.json"), append(b, '\n'), 0o644)
}

// Flush writes buffered records to disk.
func (s *Session) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, f := range s.files {
		if err := f.w.Flush(); err != nil {
			log.Printf("logs: %s/%s.jsonl: %v", s.Dir, name, err)
		}
	}
}

// Close flushes and closes every file. It is idempotent.
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.done)
	for _, f := range s.files {
		f.w.Flush()
		f.f.Close()
	}
	s.files = nil
}
