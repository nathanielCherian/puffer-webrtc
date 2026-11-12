// Package session owns one viewer: its PeerConnection, the ffmpeg decode
// pipeline, the x264 encoder driven by GCC, and every log stream.
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/interceptor/pkg/stats"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"puffer-webrtc-v2/internal/cctrace"
	"puffer-webrtc-v2/internal/logs"
	mediasrc "puffer-webrtc-v2/internal/media"
	"puffer-webrtc-v2/internal/probe"
	"puffer-webrtc-v2/internal/rtcplog"
	"puffer-webrtc-v2/internal/simulcast"
	"puffer-webrtc-v2/internal/x264"
)

// ErrTooManySessions is returned when --max-sessions is reached.
var ErrTooManySessions = errors.New("too many active sessions")

// ErrBadOffer wraps problems with the offer request itself (reported as 400).
var ErrBadOffer = errors.New("bad offer")

// ErrNoSession is returned for uploads to an unknown session.
var ErrNoSession = errors.New("no such session")

// Config holds per-session tunables (set from flags).
type Config struct {
	StartBitrate, MinBitrate, MaxBitrate int // bps, GCC limits
	AudioBitrate                         int // bps, Opus
	Headroom                             float64
	KeyIntSec                            float64 // 0 = IDR only on PLI/FIR
	X264Preset                           string
	X264Threads                          int
	SnapshotInterval                     time.Duration
	PCStatsEvery                         int // include pc.GetStats() every N snapshots (0 = never)
	OWDInterval                          time.Duration
	OWDSize                              int // beacon bytes (13..1100)
	LogDir                               string
	ConnectTimeout                       time.Duration
	MaxSessions                          int
	ICEServers                           []webrtc.ICEServer
}

// Manager creates and tracks sessions.
type Manager struct {
	cfg      Config
	info     *mediasrc.Info
	settings webrtc.SettingEngine
	host     map[string]any
	hub      *simulcast.Hub // nil unless --simulcast

	mu       sync.Mutex
	sessions map[string]*Session // live
	recent   map[string]*Session // closed, still accepting late client uploads
}

// NewManager creates a manager. settings is copied into every session's API.
func NewManager(cfg Config, info *mediasrc.Info, settings webrtc.SettingEngine, host map[string]any) *Manager {
	return &Manager{cfg: cfg, info: info, settings: settings, host: host, sessions: map[string]*Session{}, recent: map[string]*Session{}}
}

// ActiveSessions returns the number of live sessions.
func (m *Manager) ActiveSessions() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// Get returns a live or recently closed session.
func (m *Manager) Get(id string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sessions[id]; s != nil {
		return s
	}
	return m.recent[id]
}

// OfferRequest is the body of POST /offer.
type OfferRequest struct {
	SDP    string          `json:"sdp"`
	Type   string          `json:"type"`
	Client json.RawMessage `json:"client,omitempty"` // arbitrary client metadata, logged
	// Source is "" (or "file"): decode and encode the video file for this session
	// alone; or "simulcast": forward the shared pre-encoded renditions.
	Source string `json:"source,omitempty"`
	// Padding is "" (or "fill"): pad up to the whole GCC target; or "probe": pad
	// only to probe for the next simulcast layer. Simulcast sessions only.
	Padding string `json:"padding,omitempty"`
}

// OfferResponse is returned from POST /offer.
type OfferResponse struct {
	SDP       string `json:"sdp"`
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
	LogDir    string `json:"logDir"`
}

// Session is one viewer.
type Session struct {
	id      string
	mgr     *Manager
	cfg     Config
	info    *mediasrc.Info
	created time.Time
	ctx     context.Context
	cancel  context.CancelFunc
	log     *logs.Session

	pc          *webrtc.PeerConnection
	bwe         cc.BandwidthEstimator
	tracer      *cctrace.Tracer // same object as bwe; used to read per-frame RTP timestamps
	statsGetter stats.Getter
	videoTrack  *webrtc.TrackLocalStaticSample // file source: pion packetizes
	videoRTP    *webrtc.TrackLocalStaticRTP    // simulcast source: the session packetizes (see simulcast.go)
	audioTrack  *webrtc.TrackLocalStaticSample
	sim         *simState // set for simulcast sessions
	videoSSRC   uint32
	audioSSRC   uint32
	rtcp        *rtcplog.Observer
	resends     *resendLogger

	enc      *x264.Encoder
	encStats *encoderStats
	pipe     atomic.Pointer[mediasrc.Pipeline]
	forceIDR atomic.Bool
	started  atomic.Bool
	dc       atomic.Pointer[webrtc.DataChannel] // "telemetry": hello, server stats, server events
	owd      owdState                           // "owd": beacons and pings

	clientUploads atomic.Int64
	clientRecords atomic.Int64
	clientBytes   atomic.Int64

	closeOnce sync.Once
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// HandleOffer creates a session from a browser offer and returns the answer
// (with all ICE candidates included; no trickle).
func (m *Manager) HandleOffer(ctx context.Context, req OfferRequest, remoteAddr string) (*OfferResponse, error) {
	m.mu.Lock()
	if m.cfg.MaxSessions > 0 && len(m.sessions) >= m.cfg.MaxSessions {
		m.mu.Unlock()
		return nil, ErrTooManySessions
	}
	m.mu.Unlock()
	if req.SDP == "" {
		return nil, fmt.Errorf("%w: empty sdp", ErrBadOffer)
	}

	info := m.info
	var sim *simState
	switch req.Source {
	case "", SourceFile:
	case SourceSimulcast:
		var err error
		if info, err = m.simulcastInfo(); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrBadOffer, err)
		}
		if sim, err = m.newSimState(req); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%w: unknown source %q", ErrBadOffer, req.Source)
	}

	s := &Session{id: newID(), mgr: m, cfg: m.cfg, info: info, sim: sim, created: time.Now(), encStats: newEncoderStats()}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	var err error
	if s.log, err = logs.NewSession(m.cfg.LogDir, s.id); err != nil {
		return nil, fmt.Errorf("create log: %w", err)
	}
	s.rtcp = rtcplog.New(func(kind string, t time.Time, data any) { s.log.LogAt("rtcp", t, kind, data) })
	s.resends = newResendLogger(func(kind string, t time.Time, data any) { s.log.LogAt("rtcp", t, kind, data) })
	s.event("session", "created")

	if err := s.setup(ctx, req); err != nil {
		s.Close("setup failed: " + err.Error())
		return nil, err
	}
	m.mu.Lock()
	m.sessions[s.id] = s
	m.mu.Unlock()

	// Give up if ICE/DTLS never completes.
	go func() {
		select {
		case <-s.ctx.Done():
		case <-time.After(m.cfg.ConnectTimeout):
			if !s.started.Load() {
				s.Close("connect timeout")
			}
		}
	}()

	ld := s.pc.LocalDescription()
	_ = s.log.WriteMeta(map[string]any{
		"sessionId": s.id, "createdAt": s.created.UTC().Format(time.RFC3339Nano), "createdMs": logs.Ms(s.created),
		"remoteAddr": remoteAddr, "client": req.Client, "media": s.info, "config": m.cfg, "host": m.host,
		"source": s.source(), "simulcast": s.sim.describe(),
		"videoSsrc": s.videoSSRC, "audioSsrc": s.audioSSRC,
		"offer": req.SDP, "answer": ld.SDP,
		"files": map[string]string{
			"events.jsonl":    "server state changes (pc/ice/dtls states, media start, keyframe requests, close)",
			"rtcp.jsonl":      "every RTCP packet received from the browser (kind = packet type: rr, nack, pli, fir, remb, twcc, xr, sdes, bye, other) and every NACK retransmission actually sent (kind = retransmit)",
			"cc.jsonl":        "congestion controller inputs/outputs: config, stream, sent (every RTP packet with send time and TWCC seq), feedback (every TWCC packet decoded + raw), target (every target change)",
			"encoder.jsonl":   "every encoded frame (size, keyframe, encode time, configured bitrate, CC target, rtpTs = RTP timestamp joining it to cc.jsonl sent and client.jsonl frame records) and every encoder reconfiguration",
			"snapshots.jsonl": "periodic server snapshot (CC target and stats, encoder window rates, RTCP counters, RTP outbound/remote-inbound stats, pion GetStats every N)",
			"owd.jsonl":       "server-side view of the one-way-delay channel: every ping received (client t1, server t2/t3)",
			"client.jsonl":    "everything the browser uploaded (t = server receive time, tClient = browser Date.now(); kind = stats, event, frame, owdBeacon, owdPing, owdSync, qoe, bye, ...)",
		},
	})
	log.Printf("[%s] new session from %s (log: %s)", s.id, remoteAddr, s.log.Dir)
	return &OfferResponse{SDP: ld.SDP, Type: ld.Type.String(), SessionID: s.id, LogDir: s.log.Dir}, nil
}

const h264Fmtp = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"

func registerCodecs(me *webrtc.MediaEngine) error {
	videoFB := []webrtc.RTCPFeedback{
		{Type: "goog-remb"}, {Type: "ccm", Parameter: "fir"}, {Type: "nack"},
		{Type: "nack", Parameter: "pli"}, {Type: webrtc.TypeRTCPFBTransportCC},
	}
	for _, c := range []webrtc.RTPCodecParameters{
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
			SDPFmtpLine: h264Fmtp, RTCPFeedback: videoFB}, PayloadType: 102},
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f", RTCPFeedback: videoFB}, PayloadType: 127},
	} {
		if err := me.RegisterCodec(c, webrtc.RTPCodecTypeVideo); err != nil {
			return err
		}
	}
	return me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
			SDPFmtpLine: "minptime=10;useinbandfec=1", RTCPFeedback: []webrtc.RTCPFeedback{{Type: webrtc.TypeRTCPFBTransportCC}}},
		PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio)
}

func (s *Session) setup(ctx context.Context, req OfferRequest) error {
	me := &webrtc.MediaEngine{}
	if err := registerCodecs(me); err != nil {
		return err
	}
	ir := &interceptor.Registry{}

	// Interceptors wrap each other in registration order, so an outgoing packet
	// flows through them in reverse: RTCP reports -> NACK buffer -> resend
	// logger -> stats -> TWCC sequence numbering -> CC tracer -> GCC pacer -> wire.
	bweCh := make(chan cc.BandwidthEstimator, 1)
	ccFactory, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
		est, err := gcc.NewSendSideBWE(
			gcc.SendSideBWEInitialBitrate(s.cfg.StartBitrate),
			gcc.SendSideBWEMinBitrate(s.cfg.MinBitrate),
			gcc.SendSideBWEMaxBitrate(s.cfg.MaxBitrate),
		)
		if err != nil {
			return nil, err
		}
		s.log.Log("cc", "config", map[string]any{
			"algorithm": "pion send-side GCC (loss + delay)", "pacer": "leaky bucket",
			"initialBitrate": s.cfg.StartBitrate, "minBitrate": s.cfg.MinBitrate, "maxBitrate": s.cfg.MaxBitrate,
			"pion": s.mgr.host["pion"],
		})
		var bwe cc.BandwidthEstimator = cctrace.New(est, func(kind string, t time.Time, data any) { s.log.LogAt("cc", t, kind, data) })
		if s.sim != nil {
			// Outermost, so the tracer logs probes as GCC counted them (payload
			// bytes, probe=true) before they turn into RTP padding on the wire.
			bwe = probe.WithPaddingProbes(bwe)
		}
		return bwe, nil
	})
	if err != nil {
		return err
	}
	ccFactory.OnNewPeerConnection(func(_ string, e cc.BandwidthEstimator) { bweCh <- e })
	ir.Add(ccFactory)
	if err := webrtc.ConfigureTWCCHeaderExtensionSender(me, ir); err != nil {
		return err
	}
	getterCh := make(chan stats.Getter, 1)
	statsFactory, err := stats.NewInterceptor()
	if err != nil {
		return err
	}
	statsFactory.OnNewPeerConnection(func(_ string, g stats.Getter) { getterCh <- g })
	ir.Add(statsFactory)
	ir.Add(s.resends)
	if err := webrtc.ConfigureNack(me, ir); err != nil {
		return err
	}
	if err := webrtc.ConfigureRTCPReports(ir); err != nil {
		return err
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(s.mgr.settings))
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: s.cfg.ICEServers})
	if err != nil {
		return err
	}
	s.pc = pc
	s.statsGetter = <-getterCh
	s.bwe = <-bweCh
	inner := s.bwe
	if p, ok := inner.(*probe.PaddingProbes); ok {
		inner = p.Unwrap()
	}
	s.tracer, _ = inner.(*cctrace.Tracer)
	if err := s.addTracks(); err != nil {
		return err
	}

	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		s.event("pcState", st.String())
		switch st {
		case webrtc.PeerConnectionStateConnected:
			if s.started.CompareAndSwap(false, true) {
				if s.sim != nil {
					go s.runSimulcast()
				} else {
					go s.run()
				}
			}
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			s.Close("peer connection " + st.String())
		}
	})
	pc.OnICEConnectionStateChange(func(st webrtc.ICEConnectionState) { s.event("iceState", st.String()) })
	pc.OnICEGatheringStateChange(func(st webrtc.ICEGatheringState) { s.event("iceGatheringState", st.String()) })
	pc.OnSignalingStateChange(func(st webrtc.SignalingState) { s.event("signalingState", st.String()) })
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		switch dc.Label() {
		case "telemetry":
			dc.OnOpen(func() {
				s.dc.Store(dc)
				s.event("dataChannel", "telemetry open")
				s.send(map[string]any{
					"type": "hello", "sessionId": s.id, "serverTimeMs": logs.NowMs(),
					"media": s.info, "config": s.cfg, "host": s.mgr.host,
					"source": s.source(), "simulcast": s.sim.describe(),
					"videoSsrc": s.videoSSRC, "audioSsrc": s.audioSSRC, "logDir": s.log.Dir,
				})
			})
			dc.OnClose(func() { s.event("dataChannel", "telemetry closed") })
			dc.OnMessage(func(msg webrtc.DataChannelMessage) { s.onClientMessage(msg.Data) })
		case "owd":
			dc.OnOpen(func() {
				s.event("dataChannel", "owd open")
				go s.owdBeaconLoop(dc)
			})
			dc.OnClose(func() { s.event("dataChannel", "owd closed") })
			dc.OnMessage(func(msg webrtc.DataChannelMessage) { s.onOwdMessage(dc, msg.Data) })
		default:
			s.event("dataChannel", "unexpected label "+dc.Label())
		}
	})

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: req.SDP}); err != nil {
		return fmt.Errorf("set remote description: %w", err)
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("create answer: %w", err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		return fmt.Errorf("set local description: %w", err)
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(10 * time.Second):
		return errors.New("ICE gathering timed out")
	}
	return nil
}

// source names the video source of this session ("file" or "simulcast").
func (s *Session) source() string {
	if s.sim != nil {
		return SourceSimulcast
	}
	return SourceFile
}

// addTracks adds the H264 and Opus tracks and starts reading RTCP for each.
func (s *Session) addTracks() error {
	var err error
	var videoSender *webrtc.RTPSender
	if s.sim != nil {
		videoSender, err = s.addSimulcastVideoTrack()
	} else {
		s.videoTrack, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: h264Fmtp,
		}, "video", "puffer")
		if err != nil {
			return err
		}
		videoSender, err = s.pc.AddTrack(s.videoTrack)
	}
	if err != nil {
		return err
	}
	s.audioTrack, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
	}, "audio", "puffer")
	if err != nil {
		return err
	}
	audioSender, err := s.pc.AddTrack(s.audioTrack)
	if err != nil {
		return err
	}
	s.videoSSRC = uint32(videoSender.GetParameters().Encodings[0].SSRC)
	s.audioSSRC = uint32(audioSender.GetParameters().Encodings[0].SSRC)
	s.rtcp.AddLocal(s.videoSSRC, "video", 90000)
	s.rtcp.AddLocal(s.audioSSRC, "audio", 48000)
	// Interceptors only see RTCP when the application reads it.
	go s.readRTCP(videoSender, s.videoSSRC)
	go s.readRTCP(audioSender, s.audioSSRC)
	return nil
}

func (s *Session) readRTCP(sender *webrtc.RTPSender, ssrc uint32) {
	for {
		pkts, _, err := sender.ReadRTCP()
		if err != nil {
			return
		}
		if s.rtcp.Observe(ssrc, pkts) && ssrc == s.videoSSRC {
			if s.forceIDR.CompareAndSwap(false, true) {
				s.event("keyframeRequest", "PLI/FIR received, forcing IDR")
			}
		}
	}
}

// run starts media once the PeerConnection is connected.
func (s *Session) run() {
	var err error
	keyint := 0
	if s.cfg.KeyIntSec > 0 {
		keyint = int(math.Round(s.cfg.KeyIntSec * s.info.FPS))
	}
	target := s.bwe.GetTargetBitrate()
	initial := s.videoBitrateFor(target)
	s.enc, err = x264.New(x264.Config{
		Width: s.info.Width, Height: s.info.Height, FPSNum: s.info.FPSNum, FPSDen: s.info.FPSDen,
		BitrateBps: initial, KeyInt: keyint, Threads: s.cfg.X264Threads, Preset: s.cfg.X264Preset,
	})
	if err != nil {
		s.Close("encoder: " + err.Error())
		return
	}
	go func() {
		<-s.ctx.Done()
		s.enc.Close()
	}()
	s.encStats.bitrate(target, initial)
	s.log.Log("encoder", "reconfig", map[string]any{"ccTarget": target, "videoTarget": initial, "applied": s.enc.Bitrate(), "initial": true,
		"preset": s.cfg.X264Preset, "threads": s.cfg.X264Threads, "keyint": keyint, "headroom": s.cfg.Headroom, "audioBitrate": s.cfg.AudioBitrate})
	pipe, err := mediasrc.StartPipeline(s.ctx, s.info, s.cfg.AudioBitrate)
	if err != nil {
		s.Close("pipeline: " + err.Error())
		return
	}
	s.pipe.Store(pipe)
	s.event("media", "started")

	start := time.Now()
	go s.videoLoop(pipe, start)
	go s.audioLoop(pipe, start)
	go s.bitrateLoop()
	go s.snapshotLoop()
}

// videoBitrateFor converts a CC target (all streams, incl. overhead) into an encoder target.
func (s *Session) videoBitrateFor(target int) int {
	v := int(float64(target)*s.cfg.Headroom) - s.cfg.AudioBitrate
	if v < 30_000 {
		v = 30_000
	}
	return v
}

func (s *Session) bitrateLoop() {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		target := s.bwe.GetTargetBitrate()
		want := s.videoBitrateFor(target)
		cur := s.enc.Bitrate()
		if cur > 0 && math.Abs(float64(want-cur))/float64(cur) < 0.03 {
			continue
		}
		if err := s.enc.SetBitrate(want); err != nil {
			log.Printf("[%s] set bitrate: %v", s.id, err)
			continue
		}
		s.encStats.bitrate(target, s.enc.Bitrate())
		s.log.Log("encoder", "reconfig", map[string]any{"ccTarget": target, "videoTarget": want, "applied": s.enc.Bitrate(), "previous": cur})
	}
}

func (s *Session) videoLoop(pipe *mediasrc.Pipeline, start time.Time) {
	frameDur := time.Duration(float64(time.Second) * float64(s.info.FPSDen) / float64(s.info.FPSNum))
	buf := make([]byte, s.enc.FrameSize())
	pending := time.Duration(0)
	var prevRtpTs uint32
	havePrev := false
	for n := 0; ; n++ {
		if err := pipe.ReadVideoFrame(buf); err != nil {
			s.Close("video read: " + err.Error())
			return
		}
		if !sleepUntil(s.ctx, start.Add(time.Duration(n)*frameDur)) {
			return
		}
		pending += frameDur
		forced := s.forceIDR.Swap(false)
		t0 := time.Now()
		f, err := s.enc.Encode(buf, forced)
		encDur := time.Since(t0)
		if err != nil {
			s.Close("encode: " + err.Error())
			return
		}
		if f == nil {
			s.encStats.skipped()
			s.log.LogAt("encoder", t0, "skipped", map[string]any{"n": n})
			continue
		}
		t1 := time.Now()
		if err := s.videoTrack.WriteSample(media.Sample{Data: f.Data, Duration: pending}); err != nil {
			s.Close("write video: " + err.Error())
			return
		}
		writeDur := time.Since(t1)
		s.encStats.frame(len(f.Data), f.Keyframe, forced && f.Keyframe, encDur, writeDur)
		rec := map[string]any{
			"n": n, "bytes": len(f.Data), "key": f.Keyframe, "forced": forced && f.Keyframe,
			"encMs": float64(encDur.Microseconds()) / 1000, "writeMs": float64(writeDur.Microseconds()) / 1000,
			"cfgBps": s.enc.Bitrate(), "ccTarget": s.bwe.GetTargetBitrate(), "durMs": float64(pending.Microseconds()) / 1000,
		}
		// The RTP timestamp pion's packetizer gave this frame: the key that joins
		// the frame to its packets in cc.jsonl and to the browser's rendered
		// frames (rtpTimestamp) in client.jsonl. Once the peer connection is
		// closed WriteSample no longer reaches the interceptors, so the tracer's
		// value goes stale: only log it when it advanced.
		if s.tracer != nil {
			if ts, ok := s.tracer.LastTimestamp(s.videoSSRC); ok && (!havePrev || ts != prevRtpTs) {
				rec["rtpTs"] = ts
				prevRtpTs, havePrev = ts, true
			}
		}
		s.log.LogAt("encoder", t0, "frame", rec)
		pending = 0
	}
}

func (s *Session) audioLoop(pipe *mediasrc.Pipeline, start time.Time) {
	r, err := pipe.OggReader()
	if err != nil {
		s.Close("audio open: " + err.Error())
		return
	}
	var lastGranule uint64
	var mediaT time.Duration
	for {
		page, hdr, err := r.ParseNextPage()
		if err != nil {
			s.Close("audio read: " + err.Error())
			return
		}
		pipe.AudioPages.Add(1)
		if len(page) >= 8 && string(page[:8]) == "OpusTags" {
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
		if !sleepUntil(s.ctx, start.Add(mediaT)) {
			return
		}
		mediaT += dur
		if err := s.audioTrack.WriteSample(media.Sample{Data: page, Duration: dur}); err != nil {
			s.Close("write audio: " + err.Error())
			return
		}
	}
}

func sleepUntil(ctx context.Context, t time.Time) bool {
	d := time.Until(t)
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *Session) snapshotLoop() {
	t := time.NewTicker(s.cfg.SnapshotInterval)
	defer t.Stop()
	for tick := 0; ; tick++ {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		full := s.cfg.PCStatsEvery > 0 && tick%s.cfg.PCStatsEvery == 0
		snap := s.snapshot(full)
		s.log.Log("snapshots", "snapshot", snap)
		// The page gets the same snapshot without the bulky pion stats.
		msg := map[string]any{"type": "server-stats"}
		for k, v := range snap {
			if k != "pcStats" {
				msg[k] = v
			}
		}
		s.send(msg)
	}
}

// snapshot gathers everything the server knows about this session right now.
func (s *Session) snapshot(full bool) map[string]any {
	target := s.bwe.GetTargetBitrate()
	ccStats := map[string]any{"targetBitrate": target}
	for k, v := range s.bwe.GetStats() {
		ccStats[k] = v
	}
	enc := s.encStats.window()
	enc["width"], enc["height"], enc["fps"] = s.info.Width, s.info.Height, s.info.FPS
	snap := map[string]any{
		"sessionId": s.id,
		"uptimeSec": time.Since(s.created).Seconds(),
		"state": map[string]any{
			"pc": s.pc.ConnectionState().String(), "ice": s.pc.ICEConnectionState().String(), "signaling": s.pc.SignalingState().String(),
		},
		"cc":      ccStats,
		"encoder": enc,
		"rtcp":    s.rtcp.Snapshot(),
		"rtp":     map[string]any{"video": s.interceptorStats(s.videoSSRC), "audio": s.interceptorStats(s.audioSSRC)},
		"resends": s.resends.Snapshot(),
		"owd":     s.owd.snapshot(),
		"client":  map[string]any{"uploads": s.clientUploads.Load(), "records": s.clientRecords.Load(), "bytes": s.clientBytes.Load()},
	}
	if p := s.pipe.Load(); p != nil {
		pos, loops := p.MediaPosition()
		snap["media"] = map[string]any{"positionSec": pos, "loops": loops, "framesRead": p.FramesRead.Load(), "audioPages": p.AudioPages.Load()}
	}
	if s.sim != nil {
		sim := s.simSnapshot()
		media := s.simMediaSnapshot()
		// The forwarded layer's measured size and rate, as the file source's encoder reports.
		for _, l := range media["layers"].([]simulcast.LayerStatus) {
			if l.Name == sim["layer"] {
				enc["width"], enc["height"], enc["fps"] = l.Width, l.Height, l.FPS
				enc["layerMeasuredBitrate"] = l.MeasuredBps
			}
		}
		enc["source"] = "shared simulcast renditions (no per-session encoder)"
		snap["simulcast"] = sim
		snap["media"] = media
	}
	if sctp := s.pc.SCTP(); sctp != nil && sctp.Transport() != nil && sctp.Transport().ICETransport() != nil {
		if pair, err := sctp.Transport().ICETransport().GetSelectedCandidatePair(); err == nil && pair != nil {
			snap["selectedCandidatePair"] = map[string]any{"local": pair.Local.String(), "remote": pair.Remote.String()}
		}
		snap["state"].(map[string]any)["dtls"] = sctp.Transport().State().String()
	}
	if dc := s.dc.Load(); dc != nil {
		snap["dataChannelBufferedBytes"] = dc.BufferedAmount()
	}
	if full {
		snap["pcStats"] = s.pc.GetStats()
	}
	return snap
}

// interceptorStats splits pion's stats.Stats into its parts: the embedded
// structs have overlapping field names that encoding/json would silently drop.
func (s *Session) interceptorStats(ssrc uint32) map[string]any {
	st := s.statsGetter.Get(ssrc)
	if st == nil {
		return nil
	}
	ri := st.RemoteInboundRTPStreamStats
	return map[string]any{
		"outbound":      st.OutboundRTPStreamStats,
		"remoteInbound": ri,
		"rttMs":         float64(ri.RoundTripTime.Microseconds()) / 1000,
		"jitterMs":      ri.Jitter * 1000,
		"fractionLost":  ri.FractionLost,
	}
}

// onClientMessage handles the telemetry DataChannel: clock pings are answered
// immediately; anything else is logged to client.jsonl.
func (s *Session) onClientMessage(data []byte) {
	var head struct {
		Type string  `json:"type"`
		ID   int64   `json:"id"`
		T1   float64 `json:"t1"`
		T    float64 `json:"t"`
	}
	_ = json.Unmarshal(data, &head)
	if head.Type == "clock-ping" {
		s.send(map[string]any{"type": "clock-pong", "id": head.ID, "t1": head.T1, "t2": logs.NowMs()})
		return
	}
	if head.Type == "" {
		head.Type = "unknown"
	}
	s.log.Write("client", clientRecord{T: logs.NowMs(), TClient: head.T, Kind: head.Type, Via: "dc", Data: json.RawMessage(data)})
}

// clientRecord is one line of client.jsonl.
type clientRecord struct {
	T       float64         `json:"t"`              // server receive time
	TClient float64         `json:"tClient"`        // browser Date.now() when the record was made
	Perf    float64         `json:"perf,omitempty"` // browser performance.now() when the record was made
	Kind    string          `json:"kind"`
	Via     string          `json:"via,omitempty"` // "http" (periodic upload) or "dc"
	Data    json.RawMessage `json:"data"`
}

// AppendClient stores a batch uploaded by the browser: {"records": [{"t", "kind", "data"}, ...]}.
func (s *Session) AppendClient(body []byte) (int, error) {
	var batch struct {
		Records []struct {
			T    float64         `json:"t"`
			Perf float64         `json:"perf"`
			Kind string          `json:"kind"`
			Data json.RawMessage `json:"data"`
		} `json:"records"`
	}
	if err := json.Unmarshal(body, &batch); err != nil {
		return 0, err
	}
	now := logs.NowMs()
	for _, r := range batch.Records {
		s.log.Write("client", clientRecord{T: now, TClient: r.T, Perf: r.Perf, Kind: r.Kind, Via: "http", Data: r.Data})
	}
	s.clientUploads.Add(1)
	s.clientRecords.Add(int64(len(batch.Records)))
	s.clientBytes.Add(int64(len(body)))
	return len(batch.Records), nil
}

// send marshals v and sends it on the telemetry DataChannel, dropping the
// message if the channel is backed up (telemetry must not starve media).
func (s *Session) send(v any) {
	dc := s.dc.Load()
	if dc == nil || dc.ReadyState() != webrtc.DataChannelStateOpen {
		return
	}
	if dc.BufferedAmount() > 1<<20 {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("[%s] marshal telemetry: %v", s.id, err)
		return
	}
	_ = dc.SendText(string(b))
}

func (s *Session) event(kind, value string) {
	ev := map[string]any{"t": logs.NowMs(), "kind": kind, "value": value}
	s.log.Log("events", kind, map[string]any{"value": value})
	s.send(map[string]any{"type": "server-event", "event": ev})
}

// Close tears the session down; safe to call multiple times. The log stays
// open for a while so the browser's final upload still lands.
func (s *Session) Close(reason string) {
	s.closeOnce.Do(func() {
		log.Printf("[%s] closing: %s", s.id, reason)
		s.event("closed", reason)
		s.cancel()
		if p := s.pipe.Load(); p != nil {
			p.Close()
		}
		if s.sim != nil && s.sim.sub != nil {
			s.mgr.hub.Unsubscribe(s.sim.sub)
		}
		if s.pc != nil {
			_ = s.pc.Close()
		}
		s.log.Flush()
		s.mgr.mu.Lock()
		delete(s.mgr.sessions, s.id)
		s.mgr.recent[s.id] = s
		s.mgr.mu.Unlock()
		go func() {
			time.Sleep(20 * time.Second)
			s.log.Close()
			s.mgr.mu.Lock()
			delete(s.mgr.recent, s.id)
			s.mgr.mu.Unlock()
		}()
	})
}

// CloseAll shuts down every session (server shutdown) and closes their logs.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	all := make([]*Session, 0, len(m.sessions)+len(m.recent))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	for _, s := range m.recent {
		all = append(all, s)
	}
	m.mu.Unlock()
	for _, s := range all {
		s.Close("server shutdown")
		s.log.Close()
	}
}
