// Command admin serves the log dashboard: a page that lists the per-session
// log directories written by cmd/server and shows one as an overview, charts,
// a record browser and meta.json. It only reads files, needs no cgo and no
// network ports besides its own, so it can run on the measurement host or on
// any machine holding a copy of the log directory.
package main

import (
	"errors"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"puffer-webrtc-v2/internal/admin"
	adminweb "puffer-webrtc-v2/web/admin"
)

// Every flag can also be set by an environment variable: --foo-bar => PUFFER_ADMIN_FOO_BAR.
func envName(flagName string) string {
	return "PUFFER_ADMIN_" + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

func main() {
	var (
		httpAddr   = flag.String("http-addr", ":8081", "HTTP(S) listen address")
		tlsCert    = flag.String("tls-cert", "", "TLS certificate file (enables HTTPS)")
		tlsKey     = flag.String("tls-key", "", "TLS key file")
		logDir     = flag.String("log-dir", "logs", "root directory holding the per-session log directories")
		token      = flag.String("token", "", "if set, the page and API require this token (Authorization: Bearer or ?token=)")
		liveWindow = flag.Duration("live-window", 5*time.Second, "a session without a close event whose files changed this recently is shown as live")
	)
	flag.VisitAll(func(f *flag.Flag) {
		if v, ok := os.LookupEnv(envName(f.Name)); ok {
			if err := f.Value.Set(v); err != nil {
				log.Fatalf("env %s: %v", envName(f.Name), err)
			}
		}
	})
	flag.Parse()

	if st, err := os.Stat(*logDir); err != nil || !st.IsDir() {
		log.Printf("warning: log directory %q is not readable yet (%v); the list will be empty until it exists", *logDir, err)
	}
	api := admin.New(*logDir, *token, *liveWindow)
	static, _ := fs.Sub(adminweb.FS, ".")
	mux := http.NewServeMux()
	mux.Handle("GET /api/", api)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.Handle("GET /", http.FileServerFS(static))

	srv := &http.Server{Addr: *httpAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	scheme := "http"
	if *tlsCert != "" {
		scheme = "https"
	}
	log.Printf("log dashboard: serving %s on %s, reading %s", scheme, *httpAddr, *logDir)
	if *token == "" {
		log.Printf("no --token set: anyone who can reach this port can read the logs")
	}
	var err error
	if *tlsCert != "" {
		err = srv.ListenAndServeTLS(*tlsCert, *tlsKey)
	} else {
		err = srv.ListenAndServe()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
