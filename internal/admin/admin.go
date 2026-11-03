// Package admin is the log dashboard API behind cmd/admin: it lists the
// per-session log directories under a log root and lets a browser read them as
// summaries, downsampled time series, or pages of raw records, without ever
// shipping a whole cc.jsonl to the page. It only reads files, so it can run
// next to the streaming server or anywhere a copy of the log directory lives.
package admin

import (
	"bufio"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Session directories are <UTC time>_<hex id>; anything else under the log
// root is ignored (and never addressable, which also rules out traversal).
var dirRe = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z_[0-9a-f]{4,32}$`)

// The streams a session may contain, in display order.
var streamFiles = []string{"events", "snapshots", "encoder", "cc", "rtcp", "owd", "client"}

// Handler serves the dashboard API (see New).
type Handler struct {
	root       string
	token      string
	liveWindow time.Duration
	mux        *http.ServeMux

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	sig string
	val any
}

// New returns a handler for GET /api/... over the session directories under
// root. token, if non-empty, is required as "Authorization: Bearer <token>" or
// ?token=. A session without a close event whose files changed within
// liveWindow is reported as live (the streaming server flushes every second).
func New(root, token string, liveWindow time.Duration) *Handler {
	h := &Handler{root: root, token: token, liveWindow: liveWindow, cache: map[string]cacheEntry{}}
	m := http.NewServeMux()
	m.HandleFunc("GET /api/sessions", h.listSessions)
	m.HandleFunc("GET /api/sessions/{dir}/meta", h.meta)
	m.HandleFunc("GET /api/sessions/{dir}/summary", h.summary)
	m.HandleFunc("GET /api/sessions/{dir}/series", h.series)
	m.HandleFunc("GET /api/sessions/{dir}/frames", h.frames)
	m.HandleFunc("GET /api/sessions/{dir}/records", h.records)
	m.HandleFunc("GET /api/sessions/{dir}/files/{name}", h.file)
	h.mux = m
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.token != "" {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" || got == r.Header.Get("Authorization") {
			got = r.URL.Query().Get("token")
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(h.token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "token required"})
			return
		}
	}
	h.mux.ServeHTTP(w, r)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, os.ErrNotExist):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	case errors.Is(err, errBadRequest):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}

var errBadRequest = errors.New("bad request")

// dir resolves and validates the {dir} path value.
func (h *Handler) dir(r *http.Request) (string, error) {
	name := r.PathValue("dir")
	if !dirRe.MatchString(name) {
		return "", fmt.Errorf("%w: invalid session directory", errBadRequest)
	}
	p := filepath.Join(h.root, name)
	st, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", os.ErrNotExist
	}
	return p, nil
}

func idOf(dir string) string {
	base := filepath.Base(dir)
	if i := strings.IndexByte(base, '_'); i >= 0 {
		return base[i+1:]
	}
	return base
}

// signature changes whenever any file in the directory grows or is modified,
// so cached analyses of live sessions are recomputed on the next request.
func signature(dir string) string {
	entries, _ := os.ReadDir(dir)
	var b strings.Builder
	for _, e := range entries {
		if info, err := e.Info(); err == nil {
			fmt.Fprintf(&b, "%s:%d:%d;", e.Name(), info.Size(), info.ModTime().UnixNano())
		}
	}
	return b.String()
}

func (h *Handler) cached(key, dir string, compute func() (any, error)) (any, error) {
	sig := signature(dir)
	h.mu.Lock()
	if e, ok := h.cache[key]; ok && e.sig == sig {
		h.mu.Unlock()
		return e.val, nil
	}
	h.mu.Unlock()
	v, err := compute()
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	if len(h.cache) > 64 {
		h.cache = map[string]cacheEntry{}
	}
	h.cache[key] = cacheEntry{sig: sig, val: v}
	h.mu.Unlock()
	return v, nil
}

// ---------------------------------------------------------------------------
// Record scanning
// ---------------------------------------------------------------------------

// record is one JSON line of any stream. Data stays raw until a consumer
// decodes the kinds it cares about.
type record struct {
	T       float64         `json:"t"`
	TClient float64         `json:"tClient,omitempty"`
	Kind    string          `json:"kind"`
	Via     string          `json:"via,omitempty"`
	Data    json.RawMessage `json:"data"`
}

// scan calls fn for every record of <dir>/<name>.jsonl. A missing file is not
// an error (streams are created lazily). fn returns false to stop early.
func scan(dir, name string, fn func(line []byte, rec *record) bool) error {
	f, err := os.Open(filepath.Join(dir, name+".jsonl"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	var rec record
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		rec = record{}
		if err := json.Unmarshal(line, &rec); err != nil {
			continue // a partially written last line of a live session
		}
		if !fn(line, &rec) {
			break
		}
	}
	return sc.Err()
}

func decode(raw json.RawMessage, v any) bool { return json.Unmarshal(raw, v) == nil }

// ---------------------------------------------------------------------------
// GET /api/admin/sessions
// ---------------------------------------------------------------------------

// SessionInfo is one row of the session list.
type SessionInfo struct {
	Dir         string           `json:"dir"`
	ID          string           `json:"id"`
	CreatedAt   string           `json:"createdAt"`
	CreatedMs   float64          `json:"createdMs"`
	RemoteAddr  string           `json:"remoteAddr"`
	Client      ClientSummary    `json:"client"`
	Media       map[string]any   `json:"media,omitempty"`
	Live        bool             `json:"live"`
	Ended       bool             `json:"ended"`
	DurationSec float64          `json:"durationSec"`
	Bytes       int64            `json:"bytes"`
	Files       map[string]int64 `json:"files"`
	HasMeta     bool             `json:"hasMeta"`
	Modified    string           `json:"modified"`
}

// ClientSummary is the browser description shown in lists.
type ClientSummary struct {
	Browser   string `json:"browser"`
	Platform  string `json:"platform"`
	UserAgent string `json:"userAgent"`
	Mobile    bool   `json:"mobile"`
}

type metaFile struct {
	SessionID  string         `json:"sessionId"`
	CreatedAt  string         `json:"createdAt"`
	CreatedMs  float64        `json:"createdMs"`
	RemoteAddr string         `json:"remoteAddr"`
	Media      map[string]any `json:"media"`
	Client     struct {
		UserAgent     string `json:"userAgent"`
		UserAgentData struct {
			Brands []struct {
				Brand   string `json:"brand"`
				Version string `json:"version"`
			} `json:"brands"`
			Platform string `json:"platform"`
			Mobile   bool   `json:"mobile"`
		} `json:"userAgentData"`
	} `json:"client"`
}

func clientSummary(m *metaFile) ClientSummary {
	c := ClientSummary{UserAgent: m.Client.UserAgent, Platform: m.Client.UserAgentData.Platform, Mobile: m.Client.UserAgentData.Mobile}
	for _, b := range m.Client.UserAgentData.Brands {
		if !strings.Contains(b.Brand, "Brand") && b.Brand != "Chromium" {
			c.Browser = b.Brand + " " + b.Version
		}
	}
	if c.Browser == "" {
		for _, b := range m.Client.UserAgentData.Brands {
			if b.Brand == "Chromium" {
				c.Browser = "Chromium " + b.Version
			}
		}
	}
	ua := m.Client.UserAgent
	if c.Browser == "" && ua != "" {
		switch {
		case strings.Contains(ua, "Firefox/"):
			c.Browser = "Firefox " + after(ua, "Firefox/")
		case strings.Contains(ua, "Edg/"):
			c.Browser = "Edge " + after(ua, "Edg/")
		case strings.Contains(ua, "Chrome/"):
			c.Browser = "Chrome " + after(ua, "Chrome/")
		case strings.Contains(ua, "Safari/") && strings.Contains(ua, "Version/"):
			c.Browser = "Safari " + after(ua, "Version/")
		}
	}
	if c.Platform == "" && ua != "" {
		switch {
		case strings.Contains(ua, "Android"):
			c.Platform = "Android"
		case strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPad"):
			c.Platform = "iOS"
		case strings.Contains(ua, "Mac OS X"):
			c.Platform = "macOS"
		case strings.Contains(ua, "Windows"):
			c.Platform = "Windows"
		case strings.Contains(ua, "Linux"):
			c.Platform = "Linux"
		}
	}
	return c
}

func after(s, sep string) string {
	s = s[strings.Index(s, sep)+len(sep):]
	if i := strings.IndexAny(s, " ;)"); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[:i]
	}
	return s
}

func (h *Handler) sessionInfo(dir string) SessionInfo {
	info := SessionInfo{Dir: filepath.Base(dir), ID: idOf(dir), Files: map[string]int64{}}
	var m metaFile
	if b, err := os.ReadFile(filepath.Join(dir, "meta.json")); err == nil && json.Unmarshal(b, &m) == nil {
		info.HasMeta = true
		info.CreatedAt, info.CreatedMs, info.RemoteAddr, info.Media = m.CreatedAt, m.CreatedMs, m.RemoteAddr, m.Media
		info.Client = clientSummary(&m)
		if m.SessionID != "" {
			info.ID = m.SessionID
		}
	}
	var latest time.Time
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			continue
		}
		info.Files[e.Name()] = fi.Size()
		info.Bytes += fi.Size()
		if fi.ModTime().After(latest) {
			latest = fi.ModTime()
		}
	}
	if !latest.IsZero() {
		info.Modified = latest.UTC().Format(time.RFC3339)
	}
	if info.CreatedMs == 0 {
		if t, err := time.Parse("20060102T150405Z", strings.SplitN(filepath.Base(dir), "_", 2)[0]); err == nil {
			info.CreatedMs = float64(t.UnixMilli())
			info.CreatedAt = t.Format(time.RFC3339)
		}
	}
	// events.jsonl is tiny: its last record dates the end of the session.
	var last float64
	_ = scan(dir, "events", func(_ []byte, r *record) bool {
		last = r.T
		if r.Kind == "closed" {
			info.Ended = true
		}
		return true
	})
	info.Live = !info.Ended && !latest.IsZero() && time.Since(latest) < h.liveWindow
	if info.Live {
		info.DurationSec = time.Since(time.UnixMilli(int64(info.CreatedMs))).Seconds()
	} else if last > 0 && info.CreatedMs > 0 {
		info.DurationSec = (last - info.CreatedMs) / 1000
	}
	return info
}

func (h *Handler) listSessions(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(h.root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		httpError(w, err)
		return
	}
	out := []SessionInfo{}
	for _, e := range entries {
		if !e.IsDir() || !dirRe.MatchString(e.Name()) {
			continue
		}
		out = append(out, h.sessionInfo(filepath.Join(h.root, e.Name())))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir > out[j].Dir })
	writeJSON(w, http.StatusOK, map[string]any{"root": h.root, "sessions": out})
}

// ---------------------------------------------------------------------------
// GET .../meta, .../summary, .../series
// ---------------------------------------------------------------------------

func (h *Handler) meta(w http.ResponseWriter, r *http.Request) {
	dir, err := h.dir(r)
	if err != nil {
		httpError(w, err)
		return
	}
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		httpError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

// analysis runs (or reuses) the one scan of a session that produces the
// summary, the series and the per-frame rows.
func (h *Handler) analysis(r *http.Request) (*analysis, error) {
	dir, err := h.dir(r)
	if err != nil {
		return nil, err
	}
	v, err := h.cached("analysis:"+dir, dir, func() (any, error) { return analyze(dir, h.sessionInfo(dir)) })
	if err != nil {
		return nil, err
	}
	return v.(*analysis), nil
}

func (h *Handler) summary(w http.ResponseWriter, r *http.Request) {
	a, err := h.analysis(r)
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.summary)
}

func (h *Handler) series(w http.ResponseWriter, r *http.Request) {
	a, err := h.analysis(r)
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.series)
}

// GET .../frames?from=&to=&limit=&sort=t|e2e: per-frame latency rows in a
// time window (seconds since session creation), plus the frames summary.
func (h *Handler) frames(w http.ResponseWriter, r *http.Request) {
	a, err := h.analysis(r)
	if err != nil {
		httpError(w, err)
		return
	}
	q := r.URL.Query()
	from, hasFrom := parseFloat(q.Get("from"))
	to, hasTo := parseFloat(q.Get("to"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 5000 {
		limit = 2000
	}
	rows := make([]FrameRow, 0, limit)
	total := 0
	for i := range a.frames {
		f := &a.frames[i]
		if (hasFrom && f.T < from) || (hasTo && f.T > to) {
			continue
		}
		total++
		rows = append(rows, *f)
	}
	if q.Get("sort") == "e2e" {
		sort.SliceStable(rows, func(i, j int) bool {
			ei, ej := rows[i].E2E, rows[j].E2E
			switch {
			case ei == nil:
				return false
			case ej == nil:
				return true
			}
			return *ei > *ej
		})
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "limit": limit, "rows": rows, "summary": a.summary["frames"]})
}

// ---------------------------------------------------------------------------
// GET .../records?file=rtcp&kind=nack&q=text&from=10&to=20&offset=0&limit=100
// ---------------------------------------------------------------------------

func (h *Handler) records(w http.ResponseWriter, r *http.Request) {
	dir, err := h.dir(r)
	if err != nil {
		httpError(w, err)
		return
	}
	q := r.URL.Query()
	file := q.Get("file")
	ok := false
	for _, f := range streamFiles {
		ok = ok || f == file
	}
	if !ok {
		httpError(w, fmt.Errorf("%w: unknown file", errBadRequest))
		return
	}
	kind := q.Get("kind")
	needle := strings.ToLower(q.Get("q"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	// from/to are seconds relative to the session's creation.
	t0 := h.sessionInfo(dir).CreatedMs
	from, hasFrom := parseFloat(q.Get("from"))
	to, hasTo := parseFloat(q.Get("to"))
	kinds := map[string]int{}
	var total int
	page := []json.RawMessage{}
	err = scan(dir, file, func(line []byte, rec *record) bool {
		kinds[rec.Kind]++
		if kind != "" && rec.Kind != kind {
			return true
		}
		rel := (rec.T - t0) / 1000
		if (hasFrom && rel < from) || (hasTo && rel > to) {
			return true
		}
		if needle != "" && !strings.Contains(strings.ToLower(string(line)), needle) {
			return true
		}
		if total >= offset && len(page) < limit {
			page = append(page, json.RawMessage(append([]byte(nil), line...)))
		}
		total++
		return true
	})
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"file": file, "kind": kind, "offset": offset, "limit": limit, "total": total,
		"kinds": kinds, "createdMs": t0, "records": page,
	})
}

func parseFloat(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	return v, err == nil
}

// GET .../files/{name}: the raw file, as a download.
func (h *Handler) file(w http.ResponseWriter, r *http.Request) {
	dir, err := h.dir(r)
	if err != nil {
		httpError(w, err)
		return
	}
	name := r.PathValue("name")
	if name != "meta.json" && !(strings.HasSuffix(name, ".jsonl") && !strings.ContainsAny(name, "/\\")) {
		httpError(w, fmt.Errorf("%w: invalid file", errBadRequest))
		return
	}
	f, err := os.Open(filepath.Join(dir, filepath.Base(name)))
	if err != nil {
		httpError(w, err)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s_%s"`, filepath.Base(dir), name))
	if st != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	}
	_, _ = io.Copy(w, f)
}
