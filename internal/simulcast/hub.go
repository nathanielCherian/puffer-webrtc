// Package simulcast ingests pre-encoded H.264 renditions (MPEG-TS over UDP,
// one port per rendition, as produced by the sfu transcoder or
// scripts/simulcast-renditions.sh) and shares them between all viewer
// sessions. Nothing here encodes video: every session forwards the same
// encoded frames and only chooses which rendition to forward. Ported from
// github.com/nathanielCherian/webrtc-test (internal/simulcast).
package simulcast

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4/pkg/media/oggreader"
)

// LayerConfig describes one rendition: where it arrives and its nominal bitrate.
type LayerConfig struct {
	Name       string `json:"name"`
	Addr       string `json:"addr"`       // UDP host:port the transcoder sends MPEG-TS to
	BitrateBps int    `json:"bitrateBps"` // nominal (encoder target) bitrate; drives layer selection
}

// ParseLayers parses "high=127.0.0.1:5001@2500k,mid=127.0.0.1:5002@800k,...".
// The result is sorted by bitrate, highest first.
func ParseLayers(spec string) ([]LayerConfig, error) {
	var layers []LayerConfig
	seen := map[string]bool{}
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, rest, ok := strings.Cut(item, "=")
		addr, rate, ok2 := strings.Cut(rest, "@")
		if !ok || !ok2 || name == "" || addr == "" {
			return nil, fmt.Errorf("simulcast layer %q: want name=host:port@bitrate", item)
		}
		bps, err := parseBitrate(rate)
		if err != nil {
			return nil, fmt.Errorf("simulcast layer %q: %v", item, err)
		}
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return nil, fmt.Errorf("simulcast layer %q: %v", item, err)
		}
		if seen[name] {
			return nil, fmt.Errorf("simulcast layer %q defined twice", name)
		}
		seen[name] = true
		layers = append(layers, LayerConfig{Name: name, Addr: addr, BitrateBps: bps})
	}
	if len(layers) == 0 {
		return nil, errors.New("no simulcast layers")
	}
	sort.SliceStable(layers, func(i, j int) bool { return layers[i].BitrateBps > layers[j].BitrateBps })
	return layers, nil
}

func parseBitrate(s string) (int, error) {
	s = strings.TrimSpace(s)
	mult := 1.0
	switch {
	case strings.HasSuffix(s, "k"), strings.HasSuffix(s, "K"):
		mult, s = 1e3, s[:len(s)-1]
	case strings.HasSuffix(s, "M"), strings.HasSuffix(s, "m"):
		mult, s = 1e6, s[:len(s)-1]
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("bad bitrate %q", s)
	}
	return int(v * mult), nil
}

// Frame is one encoded access unit of one layer. It is shared by all
// subscribers and must not be modified.
type Frame struct {
	Layer   int    // index into Hub.Layers() (0 = highest bitrate)
	PTS     int64  // 90 kHz, unwrapped; the same clock for every layer
	Data    []byte // Annex-B access unit, as encoded
	Key     bool   // contains an IDR
	Bytes   int    // bytes that go on the wire (AUD and filler NALs excluded)
	Arrival time.Time
}

// AudioSample is one Opus page, transcoded once from the source's AAC.
type AudioSample struct {
	Data     []byte
	Duration time.Duration
}

// Subscriber receives every frame of every layer plus the audio.
type Subscriber struct {
	Video   chan *Frame
	Audio   chan *AudioSample
	Dropped atomic.Int64 // frames not delivered because Video was full
	// Overflow is set when a frame was dropped; the session must wait for a keyframe.
	Overflow atomic.Bool
}

// Options configures a Hub.
type Options struct {
	AudioLayer   int    // layer whose AAC is transcoded to Opus; -1 = no audio
	AudioBitrate int    // Opus bitrate (bps)
	FFmpeg       string // ffmpeg binary for the audio transcode
}

// Hub owns the ingest of all layers and the subscriber list.
type Hub struct {
	layers []*layer
	opt    Options

	mu   sync.RWMutex
	subs map[*Subscriber]struct{}

	audioIn       chan []byte
	audioOut      chan audioPage // transcoded pages waiting for paceAudio
	audioPages    atomic.Int64
	audioDropped  atomic.Int64 // pages dropped because the pacer's backlog was full
	audioRestarts atomic.Int64
	audioLive     atomic.Bool
}

type layer struct {
	idx int
	cfg LayerConfig

	mu          sync.Mutex
	datagrams   int64
	frames      int64
	keyframes   int64
	mediaBytes  int64
	lastPTS     int64
	ptsBase     int64 // added to the 33-bit PTS to unwrap it
	havePTS     bool
	lastArrival time.Time
	lastKey     time.Time
	sps         SPSInfo
	frameDur    int64 // last plausible PTS delta (90 kHz)
	ccErrors    int64 // copied from the demuxer after each datagram
	droppedPES  int64
	win         rateWindow
	demux       *Demuxer
}

// NewHub creates a hub; call Run to start ingest.
func NewHub(layers []LayerConfig, opt Options) *Hub {
	h := &Hub{opt: opt, subs: map[*Subscriber]struct{}{}, audioIn: make(chan []byte, 256),
		audioOut: make(chan audioPage, audioBacklogPages)}
	if opt.FFmpeg == "" {
		h.opt.FFmpeg = "ffmpeg"
	}
	if opt.AudioLayer >= len(layers) {
		h.opt.AudioLayer = -1
	}
	for i, c := range layers {
		h.layers = append(h.layers, &layer{idx: i, cfg: c, frameDur: 3000})
	}
	return h
}

// Layers returns the layer configs, highest bitrate first.
func (h *Hub) Layers() []LayerConfig {
	out := make([]LayerConfig, len(h.layers))
	for i, l := range h.layers {
		out[i] = l.cfg
	}
	return out
}

// HasAudio reports whether the hub produces an Opus track.
func (h *Hub) HasAudio() bool { return h.opt.AudioLayer >= 0 }

// Subscribe registers a new receiver of all layers and audio.
func (h *Hub) Subscribe() *Subscriber {
	s := &Subscriber{Video: make(chan *Frame, 512), Audio: make(chan *AudioSample, 256)}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

// Unsubscribe stops delivery to s.
func (h *Hub) Unsubscribe(s *Subscriber) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
}

// Subscribers returns the number of active subscribers.
func (h *Hub) Subscribers() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}

// Run listens on every layer's UDP address until ctx is done.
func (h *Hub) Run(ctx context.Context) error {
	conns := make([]*net.UDPConn, len(h.layers))
	for i, l := range h.layers {
		addr, err := net.ResolveUDPAddr("udp", l.cfg.Addr)
		if err != nil {
			return err
		}
		c, err := net.ListenUDP("udp", addr)
		if err != nil {
			for _, o := range conns[:i] {
				o.Close()
			}
			return fmt.Errorf("simulcast layer %s: listen %s: %w", l.cfg.Name, l.cfg.Addr, err)
		}
		_ = c.SetReadBuffer(8 << 20)
		conns[i] = c
	}
	go func() {
		<-ctx.Done()
		for _, c := range conns {
			c.Close()
		}
	}()
	if h.opt.AudioLayer >= 0 {
		go h.audioLoop(ctx)
		go h.paceAudio(ctx)
	}
	var wg sync.WaitGroup
	for i, l := range h.layers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.ingest(ctx, l, conns[i])
		}()
	}
	wg.Wait()
	return nil
}

func (h *Hub) ingest(ctx context.Context, l *layer, conn *net.UDPConn) {
	l.demux = NewDemuxer(func(p PES) { h.onPES(l, p) })
	buf := make([]byte, 65536)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("simulcast %s: read: %v", l.cfg.Name, err)
			}
			return
		}
		for off := 0; off+tsPacketSize <= n; off += tsPacketSize {
			_ = l.demux.Feed(buf[off : off+tsPacketSize])
		}
		l.mu.Lock()
		l.datagrams++
		l.ccErrors, l.droppedPES = l.demux.CCErrors, l.demux.DroppedPES
		l.mu.Unlock()
	}
}

func (h *Hub) onPES(l *layer, p PES) {
	switch p.StreamType {
	case streamTypeAAC:
		if l.idx == h.opt.AudioLayer {
			data := append([]byte(nil), p.Data...)
			select {
			case h.audioIn <- data:
			default: // transcoder stalled; drop rather than block video ingest
			}
		}
		return
	case streamTypeH264:
	default:
		return
	}
	if p.PTS < 0 || len(p.Data) == 0 {
		return
	}
	info := inspectAU(p.Data)
	now := time.Now()
	f := &Frame{Layer: l.idx, Data: p.Data, Key: info.key, Bytes: info.mediaBytes, Arrival: now}

	l.mu.Lock()
	pts := p.PTS + l.ptsBase
	if l.havePTS {
		switch d := pts - l.lastPTS; {
		case d < -(1 << 32): // 33-bit wrap
			l.ptsBase += 1 << 33
			pts += 1 << 33
		case d > 0 && d < 90000:
			l.frameDur = d
		}
	}
	l.lastPTS, l.havePTS = pts, true
	f.PTS = pts
	l.frames++
	l.mediaBytes += int64(info.mediaBytes)
	l.lastArrival = now
	l.win.add(now, info.mediaBytes)
	if info.key {
		l.keyframes++
		l.lastKey = now
	}
	if info.sps != nil {
		if s, err := ParseSPS(info.sps); err == nil {
			l.sps = s
		}
	}
	l.mu.Unlock()

	h.mu.RLock()
	for s := range h.subs {
		select {
		case s.Video <- f:
		default:
			s.Dropped.Add(1)
			s.Overflow.Store(true)
		}
	}
	h.mu.RUnlock()
}

// LayerStatus is a snapshot of one layer.
type LayerStatus struct {
	Name          string  `json:"name"`
	NominalBps    int     `json:"nominalBps"`
	MeasuredBps   float64 `json:"measuredBps"` // last ~1 s, wire bytes (no AUD/filler)
	Width         int     `json:"width"`
	Height        int     `json:"height"`
	Profile       int     `json:"profile"`
	Level         int     `json:"level"`
	FPS           float64 `json:"fps"`
	Frames        int64   `json:"frames"`
	Keyframes     int64   `json:"keyframes"`
	Datagrams     int64   `json:"datagrams"`
	CCErrors      int64   `json:"ccErrors"`
	DroppedPES    int64   `json:"droppedPes"`
	MsSinceFrame  any     `json:"msSinceFrame"`
	MsSinceKey    any     `json:"msSinceKeyframe"`
	Live          bool    `json:"live"` // a frame arrived in the last second
	LastPTS       int64   `json:"lastPts"`
	FrameDuration int64   `json:"frameDuration90k"`
}

// Status returns a snapshot of every layer, highest bitrate first.
func (h *Hub) Status() []LayerStatus {
	now := time.Now()
	out := make([]LayerStatus, len(h.layers))
	for i, l := range h.layers {
		l.mu.Lock()
		st := LayerStatus{
			Name: l.cfg.Name, NominalBps: l.cfg.BitrateBps, MeasuredBps: l.win.rate(now),
			Width: l.sps.Width, Height: l.sps.Height, Profile: l.sps.Profile, Level: l.sps.Level,
			Frames: l.frames, Keyframes: l.keyframes, Datagrams: l.datagrams,
			MsSinceFrame: sinceMs(now, l.lastArrival), MsSinceKey: sinceMs(now, l.lastKey),
			Live:    !l.lastArrival.IsZero() && now.Sub(l.lastArrival) < time.Second,
			LastPTS: l.lastPTS, FrameDuration: l.frameDur, CCErrors: l.ccErrors, DroppedPES: l.droppedPES,
		}
		if l.frameDur > 0 {
			st.FPS = 90000 / float64(l.frameDur)
		}
		l.mu.Unlock()
		out[i] = st
	}
	return out
}

// Snapshot is the hub section of telemetry and /info.
func (h *Hub) Snapshot() map[string]any {
	return map[string]any{
		"layers":        h.Status(),
		"subscribers":   h.Subscribers(),
		"audio":         h.HasAudio(),
		"audioLive":     h.audioLive.Load(),
		"audioPages":    h.audioPages.Load(),
		"audioRestarts": h.audioRestarts.Load(),
		"audioDropped":  h.audioDropped.Load(),
		"audioBacklog":  len(h.audioOut),
	}
}

func sinceMs(now, t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return float64(now.Sub(t).Microseconds()) / 1000
}

// rateWindow measures a byte rate over the last second in 100 ms buckets.
type rateWindow struct {
	buckets [10]int
	stamps  [10]int64 // bucket start (unix 100 ms units)
}

func (w *rateWindow) add(now time.Time, n int) {
	slot := now.UnixMilli() / 100
	i := slot % 10
	if w.stamps[i] != slot {
		w.stamps[i], w.buckets[i] = slot, 0
	}
	w.buckets[i] += n
}

// rate returns bits per second over the last full second.
func (w *rateWindow) rate(now time.Time) float64 {
	slot := now.UnixMilli() / 100
	total := 0
	for i := range w.buckets {
		if age := slot - w.stamps[i]; age >= 1 && age <= 10 {
			total += w.buckets[i]
		}
	}
	return float64(total * 8)
}

// audioLoop runs ffmpeg to turn the ADTS AAC from the audio layer into Opus pages,
// restarting it if it exits.
func (h *Hub) audioLoop(ctx context.Context) {
	for ctx.Err() == nil {
		err := h.runAudioTranscoder(ctx)
		h.audioLive.Store(false)
		if ctx.Err() != nil {
			return
		}
		h.audioRestarts.Add(1)
		log.Printf("simulcast audio transcoder exited: %v (restarting)", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (h *Hub) runAudioTranscoder(ctx context.Context) error {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(cctx, h.opt.FFmpeg, "-hide_banner", "-loglevel", "error",
		"-fflags", "nobuffer", "-probesize", "2048", "-analyzeduration", "0",
		"-f", "aac", "-i", "pipe:0",
		"-c:a", "libopus", "-b:a", strconv.Itoa(h.opt.AudioBitrate), "-ar", "48000", "-ac", "2",
		"-frame_duration", "20", "-application", "audio",
		"-page_duration", "20000", "-flush_packets", "1", "-f", "ogg", "pipe:1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() {
		defer stdin.Close()
		for {
			select {
			case <-cctx.Done():
				return
			case b, ok := <-h.audioIn:
				if !ok {
					return
				}
				if _, err := stdin.Write(b); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	readErr := h.readOpus(stdout)
	cancel()
	waitErr := cmd.Wait()
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		return fmt.Errorf("%v / %v: %s", readErr, waitErr, msg)
	}
	if readErr != nil {
		return readErr
	}
	return waitErr
}

func (h *Hub) readOpus(r io.Reader) error {
	ogg, _, err := oggreader.NewWith(r)
	if err != nil {
		return fmt.Errorf("ogg header: %w", err)
	}
	var lastGranule uint64
	var media time.Duration // position of this page in the transcoder's output
	for {
		page, hdr, err := ogg.ParseNextPage()
		if err != nil {
			return err
		}
		if bytes.HasPrefix(page, []byte("OpusTags")) {
			lastGranule = hdr.GranulePosition
			continue
		}
		dur := 20 * time.Millisecond
		if hdr.GranulePosition > lastGranule {
			if d := time.Duration(hdr.GranulePosition-lastGranule) * time.Second / 48000; d <= time.Second {
				dur = d
			}
		}
		lastGranule = hdr.GranulePosition
		h.audioPages.Add(1)
		h.audioLive.Store(true)
		select {
		case h.audioOut <- audioPage{a: &AudioSample{Data: page, Duration: dur}, arrival: time.Now(), media: media}:
		default:
			h.audioDropped.Add(1)
		}
		media += dur
	}
}

// audioPage is a transcoded page with the time it came out of ffmpeg and its
// position in ffmpeg's output (0 at each transcoder start).
type audioPage struct {
	a       *AudioSample
	arrival time.Time
	media   time.Duration
}

const (
	audioBacklogPages = 64 // ~1.3 s of 20 ms pages
	// audioPaceWindow is how long a page's arrival keeps setting the schedule.
	audioPaceWindow = 2 * time.Second
	// audioLateJump: a page this much later than the schedule is a stall, not a
	// burst. It goes out at once and does not move the schedule.
	audioLateJump = 100 * time.Millisecond
)

// paceAudio sends the Opus pages to subscribers at the rate of their timestamps.
// The transcoder emits them in bursts (ffmpeg encodes AAC as soon as it arrives).
// Forwarded as they come, pages arrive far from their RTP timestamps, the browser's
// audio jitter buffer grows to absorb them, and lip sync then raises the video
// jitter buffer to match.
//
// Each page's offset is its arrival time minus its media position. A page is due
// at its media position plus the largest offset of the last audioPaceWindow, which
// is the schedule of the first page of each burst, so the rest of a burst waits
// for its slot. After a stall, the late pages and the catch-up burst go out on
// arrival without moving the schedule, so the stall leaves nothing queued.
func (h *Hub) paceAudio(ctx context.Context) {
	type sample struct {
		arrival time.Time
		offset  time.Duration
	}
	var window []sample // offsets in decreasing order, so window[0] is the maximum
	epoch := time.Now() // offsets are measured from here
	lastMedia := time.Duration(-1)
	timer := time.NewTimer(0)
	<-timer.C
	for {
		var p audioPage
		select {
		case <-ctx.Done():
			return
		case p = <-h.audioOut:
		}
		if p.media <= lastMedia { // the transcoder restarted: new timeline
			window = window[:0]
		}
		lastMedia = p.media
		offset := p.arrival.Sub(epoch) - p.media
		for len(window) > 0 && p.arrival.Sub(window[0].arrival) > audioPaceWindow {
			window = window[1:]
		}
		if len(window) == 0 || offset <= window[0].offset+audioLateJump {
			for len(window) > 0 && window[len(window)-1].offset <= offset {
				window = window[:len(window)-1]
			}
			window = append(window, sample{p.arrival, offset})
		}
		due := epoch.Add(window[0].offset + p.media)
		if d := time.Until(due); d > 0 {
			timer.Reset(d)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		a := p.a
		h.mu.RLock()
		for s := range h.subs {
			select {
			case s.Audio <- a:
			default:
			}
		}
		h.mu.RUnlock()
	}
}
