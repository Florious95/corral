package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"tailscale.com/tsnet"
)

var (
	tsServer   *tsnet.Server
	tsServerMu sync.RWMutex
)

func currentTSNetServer() *tsnet.Server {
	tsServerMu.RLock()
	defer tsServerMu.RUnlock()
	return tsServer
}

func setTSNetServer(server *tsnet.Server) {
	tsServerMu.Lock()
	tsServer = server
	tsServerMu.Unlock()
}

type NetworkSelf struct {
	Hostname   string   `json:"hostname"`
	OS         string   `json:"os"`
	TailnetIPs []string `json:"tailnetIPs"`
	TSNetOn    bool     `json:"tsnetOn"`
}

type NetworkPeer struct {
	Hostname   string   `json:"hostname"`
	TailnetIPs []string `json:"tailnetIPs"`
	OS         string   `json:"os"`
	Online     bool     `json:"online"`
	HasGateway bool     `json:"hasGateway"`
}

func withCORS(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Last-Event-ID")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		handler(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}

func hostnameOrEmpty() string {
	hostname, _ := os.Hostname()
	return hostname
}

func handleNetwork(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	server := currentTSNetServer()
	if server == nil {
		if response, ok := networkViaLocalTailscale(); ok {
			writeJSON(w, http.StatusOK, response)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"mode":  "off",
			"self":  NetworkSelf{Hostname: hostnameOrEmpty(), OS: "unknown", TSNetOn: false},
			"peers": []NetworkPeer{},
			"note":  "tsnet not active and local tailscale not logged in",
		})
		return
	}
	lc, err := server.LocalClient()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "LocalClient: " + err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	status, err := lc.Status(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Status: " + err.Error()})
		return
	}
	self := NetworkSelf{TSNetOn: true}
	if status.Self != nil {
		self.Hostname = status.Self.HostName
		self.OS = status.Self.OS
		for _, ip := range status.Self.TailscaleIPs {
			self.TailnetIPs = append(self.TailnetIPs, ip.String())
		}
	}
	type rawPeer struct {
		hostname string
		ips      []string
		os       string
		online   bool
	}
	var raw []rawPeer
	for _, peer := range status.Peer {
		if peer == nil || len(peer.TailscaleIPs) == 0 {
			continue
		}
		item := rawPeer{hostname: peer.HostName, os: peer.OS, online: peer.Online}
		for _, ip := range peer.TailscaleIPs {
			item.ips = append(item.ips, ip.String())
		}
		raw = append(raw, item)
	}
	targets := make([]gatewayProbeTarget, 0, len(raw))
	for _, peer := range raw {
		if peer.online {
			targets = append(targets, gatewayProbeTarget{hostname: peer.hostname, ip: peer.ips[0]})
		}
	}
	reachable := probeGatewayPeers(targets)
	peers := make([]NetworkPeer, 0, len(raw))
	for _, peer := range raw {
		peers = append(peers, NetworkPeer{
			Hostname: peer.hostname, TailnetIPs: peer.ips, OS: peer.os, Online: peer.online,
			HasGateway: reachable[peer.hostname+"|"+peer.ips[0]],
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"mode": "path2_tsnet_embed", "self": self, "peers": peers})
}

type gatewayProbeTarget struct{ hostname, ip string }

func probeGatewayPeers(targets []gatewayProbeTarget) map[string]bool {
	type result struct {
		key   string
		reach bool
	}
	results := make(chan result, len(targets))
	for _, item := range targets {
		go func(item gatewayProbeTarget) {
			conn, err := (&net.Dialer{Timeout: 800 * time.Millisecond}).Dial("tcp", net.JoinHostPort(item.ip, "8787"))
			if err == nil {
				_ = conn.Close()
			}
			results <- result{key: item.hostname + "|" + item.ip, reach: err == nil}
		}(item)
	}
	reachable := map[string]bool{}
	deadline := time.After(1200 * time.Millisecond)
	for range targets {
		select {
		case result := <-results:
			reachable[result.key] = result.reach
		case <-deadline:
			return reachable
		}
	}
	return reachable
}

type localPeer struct {
	hostname string
	ips      []string
	os       string
	online   bool
}

func networkViaLocalTailscale() (map[string]any, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tailscale", "status", "--json").Output()
	if err != nil {
		return nil, false
	}
	var status struct {
		BackendState string
		Self         struct {
			HostName     string
			OS           string
			TailscaleIPs []string
		}
		Peer map[string]struct {
			HostName     string
			OS           string
			TailscaleIPs []string
			Online       bool
		}
	}
	if json.Unmarshal(out, &status) != nil || status.BackendState != "Running" || len(status.Self.TailscaleIPs) == 0 {
		return nil, false
	}
	self := NetworkSelf{Hostname: status.Self.HostName, OS: status.Self.OS, TailnetIPs: status.Self.TailscaleIPs, TSNetOn: false}
	var raw []localPeer
	for _, peer := range status.Peer {
		if len(peer.TailscaleIPs) > 0 {
			raw = append(raw, localPeer{hostname: peer.HostName, ips: peer.TailscaleIPs, os: peer.OS, online: peer.Online})
		}
	}
	targets := make([]gatewayProbeTarget, 0, len(raw))
	for _, peer := range raw {
		if peer.online {
			targets = append(targets, gatewayProbeTarget{hostname: peer.hostname, ip: peer.ips[0]})
		}
	}
	reachable := probeGatewayPeers(targets)
	peers := make([]NetworkPeer, 0, len(raw))
	for _, peer := range raw {
		peers = append(peers, NetworkPeer{
			Hostname: peer.hostname, TailnetIPs: peer.ips, OS: peer.os, Online: peer.online,
			HasGateway: reachable[peer.hostname+"|"+peer.ips[0]],
		})
	}
	return map[string]any{
		"mode": "path1_local_tailscale", "self": self, "peers": peers,
		"note": "using local tailscale client; tsnet not embedded",
	}, true
}

func staticDir() string {
	if dir := os.Getenv("GATEWAY_STATIC"); dir != "" {
		return dir
	}
	for _, path := range []string{"web/dist", "../web/dist"} {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			return path
		}
	}
	return ""
}

type gzipResponseWriter struct {
	http.ResponseWriter
	writer *gzip.Writer
}

func (w *gzipResponseWriter) WriteHeader(status int) {
	w.Header().Del("Content-Length")
	w.ResponseWriter.WriteHeader(status)
}
func (w *gzipResponseWriter) Write(data []byte) (int, error) {
	w.Header().Del("Content-Length")
	return w.writer.Write(data)
}
func (w *gzipResponseWriter) WriteString(text string) (int, error) {
	w.Header().Del("Content-Length")
	return io.WriteString(w.writer, text)
}

func gzipMiddleware(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !acceptsGzip(r.Header.Get("Accept-Encoding")) || strings.HasSuffix(r.URL.Path, "/stream") {
			handler.ServeHTTP(w, r)
			return
		}
		path := r.URL.Path
		compressible := path == "/" || !strings.Contains(path, ".") ||
			strings.HasSuffix(path, ".html") || strings.HasSuffix(path, ".css") ||
			strings.HasSuffix(path, ".js") || strings.HasSuffix(path, ".json") || strings.HasSuffix(path, ".svg")
		if !compressible {
			handler.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Vary", "Accept-Encoding")
		w.Header().Del("Content-Length")
		writer := gzip.NewWriter(w)
		defer writer.Close()
		handler.ServeHTTP(&gzipResponseWriter{ResponseWriter: w, writer: writer}, r)
	})
}

func acceptsGzip(value string) bool {
	for _, item := range strings.Split(value, ",") {
		parts := strings.Split(strings.TrimSpace(item), ";")
		if !strings.EqualFold(strings.TrimSpace(parts[0]), "gzip") {
			continue
		}
		for _, parameter := range parts[1:] {
			name, raw, found := strings.Cut(strings.TrimSpace(parameter), "=")
			if found && strings.EqualFold(strings.TrimSpace(name), "q") {
				quality, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
				if err == nil && quality <= 0 {
					return false
				}
			}
		}
		return true
	}
	return false
}

func spaHandler(dir string) http.Handler {
	files := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/" {
			path = "/index.html"
		}
		if info, err := os.Stat(dir + path); err == nil && !info.IsDir() {
			setStaticCacheHeader(w, path)
			files.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, filepathJoin(dir, "index.html"))
	})
}

func setStaticCacheHeader(w http.ResponseWriter, path string) {
	if path == "/index.html" {
		w.Header().Set("Cache-Control", "no-cache")
		return
	}
	base := path[strings.LastIndex(path, "/")+1:]
	if strings.HasPrefix(base, "index-") && (strings.HasSuffix(base, ".js") || strings.HasSuffix(base, ".css")) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
}

func filepathJoin(dir, name string) string { return strings.TrimRight(dir, "/") + "/" + name }

func startTSNet(handler http.Handler) *tsnet.Server {
	hostname := os.Getenv("RC_HOSTNAME")
	if hostname == "" {
		hostname = strings.ToLower(strings.ReplaceAll(hostnameOrEmpty(), " ", "-")) + "-rc"
	}
	server := &tsnet.Server{
		Hostname: hostname,
		AuthKey:  os.Getenv("TS_AUTHKEY"),
		Logf:     func(format string, args ...any) { log.Printf("tsnet: "+format, args...) },
	}
	setTSNetServer(server)
	go func() {
		listener, err := server.Listen("tcp", ":8787")
		if err != nil {
			log.Printf("tsnet: Listen failed: %v", err)
			if currentTSNetServer() == server {
				setTSNetServer(nil)
			}
			return
		}
		log.Printf("tsnet: registered as %s", hostname)
		if err := http.Serve(listener, handler); err != nil && err != net.ErrClosed {
			log.Printf("tsnet: Serve exit: %v", err)
		}
	}()
	return server
}

func gracefulHTTPShutdown(server *http.Server, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		_ = server.Close()
		return err
	}
	return nil
}

func runGateway(ctx context.Context) error {
	addr := os.Getenv("GATEWAY_ADDR")
	if addr == "" {
		addr = "0.0.0.0:8787"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", withCORS(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}))
	mux.HandleFunc("/api/network", withCORS(handleNetwork))
	mux.HandleFunc("/api/sessions", withCORS(handleSessions))
	mux.HandleFunc("/api/sessions/", withCORS(sessionsRouter))
	mux.HandleFunc("/api/session-truth", withCORS(handleSessionTruth))
	registerV2Routes(mux, v2Entries, loadV2RecordTimeline)
	engineCtx, cancelEngine := context.WithCancel(context.Background())
	engine, err := startV2EventEngine(engineCtx, v2Entries)
	if err != nil {
		cancelEngine()
		return err
	}
	if dir := staticDir(); dir != "" {
		mux.Handle("/", spaHandler(dir))
		log.Printf("static: serving %s", dir)
	} else {
		log.Printf("static: API-only mode")
	}
	handler := gzipMiddleware(mux)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		cancelEngine()
		engine.Stop()
		return err
	}
	server := &http.Server{Handler: handler}
	var embedded *tsnet.Server
	if os.Getenv("TSNET_DISABLE") != "1" {
		embedded = startTSNet(handler)
	} else {
		log.Printf("tsnet: disabled by TSNET_DISABLE=1")
	}
	log.Printf("gateway listening on %s", addr)
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()

	select {
	case err := <-serveErr:
		cancelEngine()
		engine.Stop()
		if embedded != nil {
			_ = embedded.Close()
			setTSNetServer(nil)
		}
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	case <-ctx.Done():
		log.Printf("shutdown: signal received")
	}

	if err := gracefulHTTPShutdown(server, 5*time.Second); err != nil {
		log.Printf("shutdown: HTTP forced closed after timeout: %v", err)
	}
	if embedded != nil {
		if err := embedded.Close(); err != nil {
			log.Printf("shutdown: tsnet close failed: %v", err)
		}
		setTSNetServer(nil)
	}
	cancelEngine()
	engine.Stop()
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 5*time.Second)
	if err := engine.Wait(waitCtx); err != nil {
		log.Printf("shutdown: event engine stop timed out: %v", err)
	}
	cancelWait()
	weakBindings.flush()
	if err := closedSessions.flush(); err != nil {
		log.Printf("shutdown: closed session state flush failed: %v", err)
	}
	log.Printf("shutdown: complete")
	return nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runGateway(ctx); err != nil {
		log.Printf("gateway: %v", err)
		os.Exit(1)
	}
}

func v2EventEngineBackgroundEnabled() bool {
	return os.Getenv("V2_EVENT_ENGINE_DISABLE") != "1"
}
