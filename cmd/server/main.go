// Command server streams a pre-recorded video to browsers over WebRTC with
// GCC-driven live H.264 encoding, and records everything about each
// connection (server side and browser side) into one log directory per session.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/pion/webrtc/v4"

	"puffer-webrtc-v2/internal/media"
	"puffer-webrtc-v2/internal/session"
	"puffer-webrtc-v2/internal/simulcast"
	"puffer-webrtc-v2/internal/x264"
	"puffer-webrtc-v2/web"
)

// Every flag can also be set by an environment variable: --foo-bar => PUFFER_FOO_BAR.
func envName(flagName string) string {
	return "PUFFER_" + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

func main() {
	var (
		httpAddr      = flag.String("http-addr", ":8080", "HTTP(S) listen address")
		tlsCert       = flag.String("tls-cert", "", "TLS certificate file (enables HTTPS)")
		tlsKey        = flag.String("tls-key", "", "TLS key file")
		videoPath     = flag.String("video", "media/standin.mp4", "source video file (looped)")
		publicIP      = flag.String("public-ip", "", "public IP(s) to advertise in ICE host candidates, comma-separated (cloud VMs behind 1:1 NAT)")
		udpPort       = flag.Int("udp-port", 50000, "single UDP port for all WebRTC media (0 = ephemeral ports)")
		udpBind       = flag.String("udp-bind", "", "IP address to bind the media port to (default: all interfaces)")
		stunURLs      = flag.String("stun", "", "STUN server URL(s), comma-separated")
		ipv6          = flag.Bool("ipv6", false, "also gather IPv6 candidates")
		startBitrate  = flag.Int("start-bitrate", 1_000_000, "GCC initial bitrate (bps)")
		minBitrate    = flag.Int("min-bitrate", 100_000, "GCC minimum bitrate (bps)")
		maxBitrate    = flag.Int("max-bitrate", 6_000_000, "GCC maximum bitrate (bps)")
		audioBitrate  = flag.Int("audio-bitrate", 64_000, "Opus bitrate (bps)")
		headroom      = flag.Float64("headroom", 0.9, "fraction of the CC target given to audio+video payload")
		keyintSec     = flag.Float64("keyint", 0, "seconds between periodic IDRs (0 = only on PLI/FIR)")
		preset        = flag.String("x264-preset", "veryfast", "x264 preset")
		threads       = flag.Int("x264-threads", 0, "x264 threads (0 = auto)")
		snapInt       = flag.Duration("snapshot-interval", 250*time.Millisecond, "server snapshot period (snapshots.jsonl and the page)")
		pcStatsEvery  = flag.Int("pc-stats-every", 4, "include pion pc.GetStats() every N snapshots (0 = never)")
		owdInterval   = flag.Duration("owd-interval", 20*time.Millisecond, "one-way-delay beacon interval")
		owdSize       = flag.Int("owd-size", 13, "one-way-delay beacon size in bytes (13..1100)")
		logDir        = flag.String("log-dir", "logs", "root directory for per-session log directories")
		maxSessions   = flag.Int("max-sessions", 8, "maximum concurrent viewers (each costs ~1 CPU core at 720p30)")
		simulcastSpec = flag.String("simulcast", "", "shared pre-encoded renditions to offer as the \"simulcast\" source: "+
			"name=host:port@bitrate,... (MPEG-TS over UDP, e.g. high=127.0.0.1:5001@2500k,mid=127.0.0.1:5002@800k,low=127.0.0.1:5003@250k)")
		simulcastAudio = flag.String("simulcast-audio", "", "rendition whose AAC audio is transcoded (once) to Opus for simulcast sessions; default the highest, \"none\" for no audio")
	)
	flag.VisitAll(func(f *flag.Flag) {
		if v, ok := os.LookupEnv(envName(f.Name)); ok {
			if err := f.Value.Set(v); err != nil {
				log.Fatalf("env %s: %v", envName(f.Name), err)
			}
		}
	})
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	info, err := media.Probe(ctx, *videoPath)
	if err != nil {
		log.Fatalf("probe video: %v (generate a stand-in with scripts/make-standin.sh)", err)
	}
	log.Printf("source %s: %dx%d @ %.3f fps, %.1fs, audio=%v", info.Path, info.Width, info.Height, info.FPS, info.DurationSec, info.HasAudio)

	se := webrtc.SettingEngine{}
	netTypes := []webrtc.NetworkType{webrtc.NetworkTypeUDP4}
	if *ipv6 {
		netTypes = append(netTypes, webrtc.NetworkTypeUDP6)
	}
	se.SetNetworkTypes(netTypes)
	if *udpPort > 0 {
		network := "udp4"
		if *ipv6 {
			network = "udp"
		}
		bindIP := net.ParseIP(*udpBind)
		if bindIP != nil && bindIP.IsLoopback() {
			se.SetIncludeLoopbackCandidate(true)
		}
		conn, err := net.ListenUDP(network, &net.UDPAddr{IP: bindIP, Port: *udpPort})
		if err != nil {
			log.Fatalf("listen udp %d: %v", *udpPort, err)
		}
		se.SetICEUDPMux(webrtc.NewICEUDPMux(nil, conn))
		log.Printf("WebRTC media on UDP port %d", *udpPort)
	}
	if *publicIP != "" {
		if err := se.SetICEAddressRewriteRules(webrtc.ICEAddressRewriteRule{
			External:        splitList(*publicIP),
			AsCandidateType: webrtc.ICECandidateTypeHost,
			Mode:            webrtc.ICEAddressRewriteReplace,
		}); err != nil {
			log.Fatalf("public ip: %v", err)
		}
		log.Printf("advertising public IP(s) %s", *publicIP)
	}
	var iceServers []webrtc.ICEServer
	if *stunURLs != "" {
		iceServers = append(iceServers, webrtc.ICEServer{URLs: splitList(*stunURLs)})
	}

	cfg := session.Config{
		StartBitrate: *startBitrate, MinBitrate: *minBitrate, MaxBitrate: *maxBitrate,
		AudioBitrate: *audioBitrate, Headroom: *headroom, KeyIntSec: *keyintSec,
		X264Preset: *preset, X264Threads: *threads,
		SnapshotInterval: *snapInt, PCStatsEvery: *pcStatsEvery,
		OWDInterval: *owdInterval, OWDSize: *owdSize,
		LogDir: *logDir, ConnectTimeout: 30 * time.Second, MaxSessions: *maxSessions,
		ICEServers: iceServers,
	}
	mgr := session.NewManager(cfg, info, se, hostInfo(*publicIP, *udpPort))

	// Optional shared simulcast source: pre-encoded renditions arriving as
	// MPEG-TS over UDP, demuxed once and forwarded by every simulcast session.
	var hub *simulcast.Hub
	if *simulcastSpec != "" {
		layers, err := simulcast.ParseLayers(*simulcastSpec)
		if err != nil {
			log.Fatalf("--simulcast: %v", err)
		}
		audioLayer := 0
		switch *simulcastAudio {
		case "":
		case "none":
			audioLayer = -1
		default:
			audioLayer = -1
			for i, l := range layers {
				if l.Name == *simulcastAudio {
					audioLayer = i
				}
			}
			if audioLayer < 0 {
				log.Fatalf("--simulcast-audio: no layer named %q", *simulcastAudio)
			}
		}
		hub = simulcast.NewHub(layers, simulcast.Options{AudioLayer: audioLayer, AudioBitrate: *audioBitrate})
		go func() {
			if err := hub.Run(ctx); err != nil {
				log.Fatalf("simulcast: %v", err)
			}
		}()
		mgr.SetSimulcast(hub)
		for _, l := range layers {
			log.Printf("simulcast layer %s: %d kbps from udp %s", l.Name, l.BitrateBps/1000, l.Addr)
		}
	}
	simulcastStatus := func() any {
		if hub == nil {
			return nil
		}
		return hub.Snapshot()
	}

	mux := http.NewServeMux()
	static, _ := fs.Sub(web.FS, ".")
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sessions": mgr.ActiveSessions()})
	})
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"media": info, "config": cfg, "sessions": mgr.ActiveSessions(), "simulcast": simulcastStatus()})
	})
	mux.HandleFunc("POST /offer", func(w http.ResponseWriter, r *http.Request) {
		var req session.OfferRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil || req.SDP == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid offer"})
			return
		}
		resp, err := mgr.HandleOffer(r.Context(), req, clientAddr(r))
		switch {
		case errors.Is(err, session.ErrTooManySessions):
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		case errors.Is(err, session.ErrBadOffer):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		case err != nil:
			log.Printf("offer from %s: %v", clientAddr(r), err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		default:
			writeJSON(w, http.StatusOK, resp)
		}
	})
	// The browser uploads batches of its own records here (see web/app.js).
	mux.HandleFunc("POST /client-log", func(w http.ResponseWriter, r *http.Request) {
		s := mgr.Get(r.URL.Query().Get("session"))
		if s == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": session.ErrNoSession.Error()})
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<20))
		if err != nil {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": err.Error()})
			return
		}
		n, err := s.AppendClient(body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"records": n})
	})

	srv := &http.Server{Addr: *httpAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		log.Printf("shutting down")
		mgr.CloseAll()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	scheme := "http"
	if *tlsCert != "" {
		scheme = "https"
	}
	log.Printf("serving %s on %s (logs in %s)", scheme, *httpAddr, *logDir)
	if *tlsCert != "" {
		err = srv.ListenAndServeTLS(*tlsCert, *tlsKey)
	} else {
		err = srv.ListenAndServe()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func clientAddr(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		return fwd
	}
	return r.RemoteAddr
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// hostInfo is recorded in every session's meta.json.
func hostInfo(publicIP string, udpPort int) map[string]any {
	hostname, _ := os.Hostname()
	h := map[string]any{
		"hostname": hostname, "numCPU": runtime.NumCPU(), "goos": runtime.GOOS, "goarch": runtime.GOARCH,
		"goVersion": runtime.Version(), "x264Build": x264.Build(), "publicIP": publicIP, "udpPort": udpPort,
		"startedAt": time.Now().UTC().Format(time.RFC3339),
	}
	if out, err := exec.Command("ffmpeg", "-version").Output(); err == nil {
		h["ffmpeg"] = strings.SplitN(string(out), "\n", 2)[0]
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		deps := map[string]string{}
		for _, d := range bi.Deps {
			if strings.HasPrefix(d.Path, "github.com/pion/") {
				deps[strings.TrimPrefix(d.Path, "github.com/pion/")] = d.Version
			}
		}
		h["pion"] = deps
	}
	return h
}
