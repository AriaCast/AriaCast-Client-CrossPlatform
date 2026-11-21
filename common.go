package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gen2brain/malgo"
	"github.com/gorilla/websocket"
	webview "github.com/webview/webview_go"
)

// Common Types and Globals

func writeFallbackDiagnostic(msg string) {
	p := filepath.Join(os.TempDir(), "ariacast_debug.txt")
	os.WriteFile(p, []byte(msg+"\n"), 0644)
}

// init sets up logging. ARIACAST_LOG overrides the default log file
// (ariacast.log next to the executable):
//
//	ARIACAST_LOG=off     discard all log output
//	ARIACAST_LOG=stderr  log to stderr (useful when run from a terminal)
//	ARIACAST_LOG=<path>  log to that file
func init() {
	logPath := "ariacast.log"
	exePath, exeErr := os.Executable()
	if exeErr == nil {
		logPath = filepath.Join(filepath.Dir(exePath), "ariacast.log")
	}

	switch env := os.Getenv("ARIACAST_LOG"); strings.ToLower(env) {
	case "":
	case "off", "none", "0", "false":
		log.SetOutput(io.Discard)
		return
	case "stderr":
		log.SetFlags(log.LstdFlags | log.Lmicroseconds)
		return
	default:
		logPath = env
	}

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		writeFallbackDiagnostic(fmt.Sprintf("os.Executable() = %q (err: %v)\nCould not open log file %s: %v", exePath, exeErr, logPath, err))
		return
	}

	canary := fmt.Sprintf("=== AriaCast Client starting (log path: %s) ===\n", logPath)
	if _, werr := f.WriteString(canary); werr != nil {
		writeFallbackDiagnostic(fmt.Sprintf("os.Executable() = %q (err: %v)\nLog file opened at %s but WriteString failed: %v", exePath, exeErr, logPath, werr))
		f.Close()
		return
	}
	if serr := f.Sync(); serr != nil {
		writeFallbackDiagnostic(fmt.Sprintf("Log file %s: canary write OK but Sync failed: %v", logPath, serr))
	}

	log.SetOutput(f)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Println("Logger fully initialized")
}

// Global App State
var (
	w              webview.WebView
	mu             sync.Mutex
	statusMsg      string  = "Idle"
	serverInfo     string  = "Not Connected"
	volume         float64 = 1.0
	isRunning      bool
	cancelFunc     context.CancelFunc
	selectedServer *ServerInfo // server explicitly picked from the discovery list; guarded by mu
)

// Discovery State
var (
	discoveryMu       sync.Mutex
	discoveredServers = make(map[string]ServerInfo) // keyed by server_name, mirrors the Android client
)

type Binding struct{}

func (b *Binding) Start() {
	log.Println("User requested Start")
	mu.Lock()
	if isRunning {
		mu.Unlock()
		log.Println("Start ignored: already running")
		return
	}
	isRunning = true
	ctx, cancel := context.WithCancel(context.Background())
	cancelFunc = cancel
	mu.Unlock()

	setStatus("Connecting...")
	go runAudioLoop(ctx)
}

func (b *Binding) Stop() {
	log.Println("User requested Stop")
	mu.Lock()
	if isRunning && cancelFunc != nil {
		cancelFunc() // Cancel context
		isRunning = false
	}
	mu.Unlock() // release before setStatus, which also locks mu
	setStatus("Stopped")
}

func (b *Binding) SetVolume(val float64) {
	mu.Lock()
	volume = val / 100.0
	mu.Unlock()
}

// RefreshServers clears the discovery list and any prior selection, then
// immediately runs a fresh scan, mirroring the "Refresh" button in the
// AriaCast Android app. Clearing the selection avoids streaming to a stale
// address if the previously-picked server's IP has since changed.
func (b *Binding) RefreshServers() {
	log.Println("User requested discovery refresh")
	mu.Lock()
	selectedServer = nil
	mu.Unlock()
	discoveryMu.Lock()
	discoveredServers = make(map[string]ServerInfo)
	discoveryMu.Unlock()
	setServerInfo("Not Connected")
	pushServerList()
	go scanOnce(context.Background())
}

// GetServers returns the currently known discovered servers; the UI calls
// this once on load to pick up any results found before its own JS was
// ready to receive the live pushServerList updates.
func (b *Binding) GetServers() []ServerInfo {
	return snapshotServers()
}

// SelectServer records the user's choice from the discovery list; it takes
// effect the next time Start is called.
func (b *Binding) SelectServer(name string) {
	for _, s := range snapshotServers() {
		if s.ServerName != name {
			continue
		}
		info := s
		mu.Lock()
		selectedServer = &info
		mu.Unlock()
		log.Printf("User selected server: %s (%s:%d)", info.ServerName, info.IP, info.Port)
		setServerInfo(fmt.Sprintf("Selected: %s (%s:%d)", info.ServerName, info.IP, info.Port))
		return
	}
	log.Printf("SelectServer: unknown server name %q", name)
}

func (b *Binding) Close() {
	if w != nil {
		w.Terminate()
	}
	os.Exit(0)
}

func setStatus(msg string) {
	mu.Lock()
	statusMsg = msg
	mu.Unlock()

	if w != nil {
		w.Dispatch(func() {
			// msg can carry attacker-controlled text (e.g. a server_name from a
			// UDP discovery reply), so it must go through json.Marshal rather
			// than naive quoting to avoid breaking out of the JS string literal.
			data, _ := json.Marshal(msg)
			w.Eval(fmt.Sprintf("updateStatus(%s);", data))
		})
	}
}

func setServerInfo(msg string) {
	mu.Lock()
	serverInfo = msg
	mu.Unlock()

	if w != nil {
		w.Dispatch(func() {
			data, _ := json.Marshal(msg)
			w.Eval(fmt.Sprintf("updateServerInfo(%s);", data))
		})
	}
}

// -----------------------------------------------------------------------------
// Audio Logic (Common)
// -----------------------------------------------------------------------------

const (
	DiscoveryPort         = 12888
	DefaultServerPort     = 12889
	DiscoveryMsg          = "DISCOVER_AUDIOCAST"
	DiscoveryBurstWindow  = 3 * time.Second // matches the AriaCast Android client's UDP listen window
	DiscoveryLoopInterval = 5 * time.Second // matches the AriaCast Android client's re-broadcast interval
	SampleRate            = 48000
	Channels              = 2
	AudioFormat           = malgo.FormatS16
	ProtocolFrameSize     = 3840
)

type ServerInfo struct {
	ServerName string `json:"server_name"`
	IP         string `json:"ip"`
	Port       int    `json:"port"`
}

type manualConfig struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
	Name string `json:"name"`
}

// loadManualConfig reads ariacast_config.json next to the executable.
// Returns nil if the file is absent, invalid, or still has the placeholder IP,
// in which case the caller should fall back to UDP discovery.
func loadManualConfig() *ServerInfo {
	path := "ariacast_config.json"
	exePath, exeErr := os.Executable()
	if exeErr == nil {
		path = filepath.Join(filepath.Dir(exePath), "ariacast_config.json")
	}
	log.Printf("Looking for manual config at: %s (executable path: %q, err: %v)", path, exePath, exeErr)

	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("Could not read %s: %v", path, err)
		return nil
	}

	var cfg manualConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Printf("Invalid ariacast_config.json: %v", err)
		return nil
	}

	if cfg.IP == "" || cfg.IP == "your_server_ip" {
		return nil
	}
	if cfg.Port == 0 {
		cfg.Port = DefaultServerPort
	}
	name := cfg.Name
	if name == "" {
		name = "Manual Server"
	}

	return &ServerInfo{ServerName: name, IP: cfg.IP, Port: cfg.Port}
}

func runAudioLoop(ctx context.Context) {
	var lastErr string
	fail := func(msg string) {
		lastErr = msg
		log.Println(msg)
		setStatus(msg)
	}

	defer func() {
		mu.Lock()
		isRunning = false
		mu.Unlock()
		if lastErr != "" {
			log.Printf("Stream ended after error: %s", lastErr)
			setStatus(lastErr + " (Disconnected)")
		} else {
			log.Println("Stream stopped")
			setStatus("Disconnected (Stopped)")
		}
	}()

	// 1. Resolve target server: manual override > explicit UI selection > sole discovered server
	setStatus("Resolving server...")
	server, err := resolveServer()
	if err != nil {
		fail("Error: " + err.Error())
		return
	}
	log.Printf("Using server: %s (%s:%d)", server.ServerName, server.IP, server.Port)
	setServerInfo(fmt.Sprintf("%s (%s:%d)", server.ServerName, server.IP, server.Port))
	setStatus("Server Found! Connecting...")
	defer sendPlayingState(server, false)

	// 2. Connect /audio plus the /control and /stats side channels
	sess, err := connectSession(server, handleControlMessage)
	if err != nil {
		fail("Connection failed: " + err.Error())
		return
	}
	defer func() { sess.close() }()

	// The capture callback writes to whichever /audio connection is current;
	// it is nil while reconnecting, and frames captured meanwhile are dropped.
	var current atomic.Pointer[websocket.Conn]

	// 3. Audio Capture Setup
	mctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, func(message string) {
		log.Println("malgo:", message)
	})
	if err != nil {
		fail("Audio Init Error: " + err.Error())
		return
	}
	defer mctx.Free()

	// Platform Specific Device Logic
	deviceConfig := getPlatformDeviceConfig()

	// Double buffering logic to handle frames
	frameBuffer := make([]byte, 0, ProtocolFrameSize*2)
	var bufferMutex sync.Mutex
	var loggedWriteErr bool
	var levelCallCount int

	onRecvFrames := func(pOutputSample, pInputSamples []byte, framecount uint32) {
		bufferMutex.Lock()
		defer bufferMutex.Unlock()

		c := current.Load()
		if c == nil {
			frameBuffer = frameBuffer[:0]
			return
		}

		levelCallCount++
		if levelCallCount%30 == 0 {
			var peak int16
			for i := 0; i+1 < len(pInputSamples); i += 2 {
				v := int16(binary.LittleEndian.Uint16(pInputSamples[i : i+2]))
				if v < 0 {
					v = -v
				}
				if v > peak {
					peak = v
				}
			}
			log.Printf("Captured audio level (peak): %d / 32767", peak)
		}

		// Apply gain
		mu.Lock()
		currentGain := volume
		mu.Unlock()

		if currentGain != 1.0 {
			// pInputSamples is raw byte slice (S16LE).
			for i := 0; i < len(pInputSamples); i += 2 {
				if i+1 >= len(pInputSamples) {
					break
				}
				val := int16(binary.LittleEndian.Uint16(pInputSamples[i : i+2]))
				fVal := float64(val) * currentGain
				if fVal > 32767 {
					val = 32767
				} else if fVal < -32768 {
					val = -32768
				} else {
					val = int16(fVal)
				}
				binary.LittleEndian.PutUint16(pInputSamples[i:i+2], uint16(val))
			}
		}

		frameBuffer = append(frameBuffer, pInputSamples...)

		// Send chunks of exactly ProtocolFrameSize
		for len(frameBuffer) >= ProtocolFrameSize {
			chunk := frameBuffer[:ProtocolFrameSize]
			err := c.WriteMessage(websocket.BinaryMessage, chunk)
			if err != nil {
				if !loggedWriteErr {
					loggedWriteErr = true
					log.Printf("WebSocket write error: %v", err)
				}
				// Closing the connection ends its read pump, which makes the
				// main loop below notice the drop and reconnect.
				current.CompareAndSwap(c, nil)
				c.Close()
				frameBuffer = frameBuffer[:0]
				return
			}
			frameBuffer = frameBuffer[ProtocolFrameSize:]
		}
	}

	deviceCallbacks := malgo.DeviceCallbacks{
		Data: onRecvFrames,
	}

	device, err := malgo.InitDevice(mctx.Context, deviceConfig, deviceCallbacks)
	if err != nil {
		fail("Device Init Error: " + err.Error())
		return
	}
	defer device.Uninit()

	if err := device.Start(); err != nil {
		fail("Start Error: " + err.Error())
		return
	}

	log.Println("Audio device started, capturing")

	// 4. Stream until the user stops; if the /audio connection drops,
	// reconnect with backoff. Metadata is re-sent on every (re)connect and
	// every 10s while connected, like the Android app.
	refresh := time.NewTicker(10 * time.Second)
	defer refresh.Stop()
	for {
		loggedWriteErr = false
		current.Store(sess.audio)
		sendPlayingState(server, true)
		setStatus("Streaming Active")

	connected:
		for {
			select {
			case <-ctx.Done():
				log.Println("Context cancelled, stopping audio loop")
				return
			case <-sess.dead:
				break connected
			case <-refresh.C:
				sendPlayingState(server, true)
			}
		}

		log.Println("Audio connection lost, reconnecting")
		current.Store(nil)
		sess.close()

		for attempt := 1; ; attempt++ {
			setStatus(fmt.Sprintf("Reconnecting... (attempt %d)", attempt))
			select {
			case <-ctx.Done():
				log.Println("Context cancelled while reconnecting")
				return
			case <-time.After(reconnectDelay(attempt)):
			}
			sess, err = connectSession(server, handleControlMessage)
			if err == nil {
				break
			}
			log.Printf("Reconnect attempt %d failed: %v", attempt, err)
		}
	}
}

// reconnectDelay backs off 1s, 2s, 4s, ... capped at 10s.
func reconnectDelay(attempt int) time.Duration {
	d := time.Second << min(attempt-1, 4)
	return min(d, 10*time.Second)
}

// session is one set of WebSocket connections to the receiver.
type session struct {
	audio *websocket.Conn
	dead  chan struct{} // closed when the /audio connection is gone
	close func()        // closes all connections; safe to call repeatedly
}

// connectSession opens /audio and, as in the protocol's data flow, the
// /control (receiver -> sender playback commands) and /stats (buffer
// statistics) side channels. The side channels are best-effort: streaming
// still works on /audio alone if they fail.
func connectSession(server *ServerInfo, onControl func([]byte)) (*session, error) {
	base := fmt.Sprintf("ws://%s:%d", server.IP, server.Port)
	log.Printf("Connecting to %s/audio", base)
	audio, _, err := websocket.DefaultDialer.Dial(base+"/audio", nil)
	if err != nil {
		return nil, err
	}
	log.Println("WebSocket connected")

	s := &session{audio: audio, dead: make(chan struct{})}
	conns := []*websocket.Conn{audio}

	// Read pump on /audio: logs the receiver's READY handshake and detects a
	// dropped connection (gorilla/websocket only notices while reading).
	go func() {
		defer close(s.dead)
		for {
			mt, msg, err := audio.ReadMessage()
			if err != nil {
				log.Printf("%s/audio read loop ended: %v", base, err)
				return
			}
			if mt == websocket.TextMessage {
				log.Printf("Received from %s/audio: %s", base, msg)
			}
		}
	}()

	if ctrl := dialSideChannel(base+"/control", onControl); ctrl != nil {
		conns = append(conns, ctrl)
	}
	if stats := dialSideChannel(base+"/stats", nil); stats != nil {
		conns = append(conns, stats)
	}

	var once sync.Once
	s.close = func() {
		once.Do(func() {
			for _, c := range conns {
				c.Close()
			}
		})
	}
	return s, nil
}

// dialSideChannel connects an auxiliary WebSocket (/control or /stats) and
// starts a read pump on it. gorilla/websocket only answers pings and notices
// a closed peer while something is reading, so the pump is needed even when
// the messages themselves are ignored. Text messages are logged and passed
// to onText if it is non-nil. Returns nil if the dial fails.
func dialSideChannel(u string, onText func([]byte)) *websocket.Conn {
	log.Printf("Connecting to %s", u)
	c, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		log.Printf("%s failed (non-fatal): %v", u, err)
		return nil
	}

	go func() {
		for {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				log.Printf("%s read loop ended: %v", u, err)
				return
			}
			if onText != nil && mt == websocket.TextMessage {
				log.Printf("Received from %s: %s", u, msg)
				onText(msg)
			}
		}
	}()
	return c
}

// controlCommand is a receiver -> sender /control frame, e.g.
// {"action":"seek","position_ms":12345}. Seek targets may also arrive in
// "value", which the Python server accepts as an alias.
type controlCommand struct {
	Action     string `json:"action"`
	PositionMs *int64 `json:"position_ms"`
	Value      *int64 `json:"value"`
}

// handleControlMessage forwards the receiver's playback commands to the
// local media player, the way the Android app forwards them to its media
// session. The captured audio keeps streaming either way.
func handleControlMessage(msg []byte) {
	var cmd controlCommand
	if err := json.Unmarshal(msg, &cmd); err != nil || cmd.Action == "" {
		return // e.g. {"command":...,"success":true} acknowledgements
	}
	action := strings.ToLower(cmd.Action)

	var pos int64
	if action == "seek" {
		switch {
		case cmd.PositionMs != nil:
			pos = *cmd.PositionMs
		case cmd.Value != nil:
			pos = *cmd.Value
		default:
			log.Printf("Ignoring seek without position: %s", msg)
			return
		}
	}

	// D-Bus calls can block briefly, so they run on a worker to keep the read
	// pump responsive; a single worker keeps the commands in order.
	mediaWorkerOnce.Do(func() { go mediaWorker() })
	select {
	case mediaCmds <- mediaCmd{action, pos}:
	default:
		log.Printf("Media command queue full, dropping %q", action)
	}
}

type mediaCmd struct {
	action     string
	positionMs int64
}

var (
	mediaCmds       = make(chan mediaCmd, 16)
	mediaWorkerOnce sync.Once
)

func mediaWorker() {
	for c := range mediaCmds {
		if err := forwardMediaCommand(c.action, c.positionMs); err != nil {
			log.Printf("Control action %q not forwarded: %v", c.action, err)
		}
	}
}

// sendPlayingState mirrors the AriaCast Android app's metadata POST
// (see AudioCastService.performMetadataUpdate / TrackMetadata): the receiver
// uses "isPlaying" to know a streaming session is actually active.
func sendPlayingState(server *ServerInfo, playing bool) {
	url := fmt.Sprintf("http://%s:%d/metadata", server.IP, server.Port)
	body, _ := json.Marshal(map[string]interface{}{
		"data": map[string]interface{}{
			"title":      "AriaCast Desktop",
			"artist":     nil,
			"album":      nil,
			"artworkUrl": nil,
			"durationMs": nil,
			"positionMs": nil,
			"isPlaying":  playing,
		},
	})

	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("Metadata POST to %s failed: %v", url, err)
		return
	}
	defer resp.Body.Close()
	log.Printf("Metadata POST to %s (isPlaying=%v): %s", url, playing, resp.Status)
}

// resolveServer picks the server to stream to, in priority order:
//  1. an explicit manual override configured via ariacast_config.json,
//  2. the server the user picked from the discovery list in the UI,
//  3. the sole server currently visible in the discovery list.
func resolveServer() (*ServerInfo, error) {
	if cfg := loadManualConfig(); cfg != nil {
		log.Printf("Using manual server config: %s (%s:%d)", cfg.ServerName, cfg.IP, cfg.Port)
		return cfg, nil
	}

	mu.Lock()
	sel := selectedServer
	mu.Unlock()
	if sel != nil {
		return sel, nil
	}

	servers := snapshotServers()
	switch len(servers) {
	case 0:
		return nil, fmt.Errorf("no server found - try Refresh")
	case 1:
		return &servers[0], nil
	default:
		return nil, fmt.Errorf("multiple servers found - select one")
	}
}

// startDiscoveryLoop repeatedly broadcasts an AriaCast discovery request and
// listens for replies, mirroring the Android client's DiscoveryManager: a
// burst of DiscoveryBurstWindow spent listening, then DiscoveryLoopInterval
// idle before broadcasting again. It runs for the lifetime of the app.
func startDiscoveryLoop(ctx context.Context) {
	for {
		scanOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(DiscoveryLoopInterval):
		}
	}
}

// scanOnce sends an AriaCast discovery broadcast on every broadcast-capable
// network interface and collects replies for DiscoveryBurstWindow, pushing
// each newly-seen or updated server to the UI as it arrives.
func scanOnce(ctx context.Context) {
	addrs := broadcastCapableIPv4Addrs()
	if len(addrs) == 0 {
		log.Println("discovery: no broadcast-capable interface found, falling back to wildcard bind")
		scanFromLocalAddr(ctx, nil)
		return
	}

	var wg sync.WaitGroup
	for _, addr := range addrs {
		wg.Add(1)
		go func(ip net.IP) {
			defer wg.Done()
			scanFromLocalAddr(ctx, ip)
		}(addr)
	}
	wg.Wait()
}

func broadcastCapableIPv4Addrs() []net.IP {
	ifaces, err := net.Interfaces()
	if err != nil {
		log.Printf("discovery: interface enumeration error: %v", err)
		return nil
	}

	var addrs []net.IP
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagBroadcast == 0 {
			continue
		}
		ifaceAddrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range ifaceAddrs {
			ipNet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ip4 := ipNet.IP.To4(); ip4 != nil {
				addrs = append(addrs, ip4)
			}
		}
	}
	return addrs
}

func scanFromLocalAddr(ctx context.Context, localIP net.IP) {
	pc, err := net.ListenPacket("udp4", (&net.UDPAddr{IP: localIP, Port: 0}).String())
	if err != nil {
		log.Printf("discovery: listen error on %v: %v", localIP, err)
		return
	}
	defer pc.Close()

	broadcastAddr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("255.255.255.255:%d", DiscoveryPort))
	if err != nil {
		log.Printf("discovery: resolve error: %v", err)
		return
	}
	if _, err := pc.WriteTo([]byte(DiscoveryMsg), broadcastAddr); err != nil {
		log.Printf("discovery: send error on %v: %v", localIP, err)
		return
	}
	log.Printf("discovery: broadcast sent from %v, listening for replies", localIP)

	stopRead := make(chan struct{})
	defer close(stopRead)
	go func() {
		select {
		case <-ctx.Done():
			pc.Close() // unblocks ReadFrom below if the app is shutting down
		case <-stopRead:
		}
	}()

	deadline := time.Now().Add(DiscoveryBurstWindow)
	buf := make([]byte, 1024)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		pc.SetReadDeadline(time.Now().Add(remaining))
		n, raddr, err := pc.ReadFrom(buf)
		if err != nil {
			return // timeout, or socket closed because ctx was cancelled
		}

		// Only server_name and port are trusted from the payload; the
		// sender's IP is taken from the UDP packet itself (like the Android
		// client), since a server can't reliably know its own address.
		var payload struct {
			ServerName string `json:"server_name"`
			Port       int    `json:"port"`
		}
		if err := json.Unmarshal(buf[:n], &payload); err != nil || payload.ServerName == "" || payload.Port == 0 {
			continue
		}

		host, _, err := net.SplitHostPort(raddr.String())
		if err != nil {
			continue
		}
		addOrUpdateServer(ServerInfo{ServerName: payload.ServerName, IP: host, Port: payload.Port})
	}
}

// addOrUpdateServer records a discovered server (keyed by name, like the
// Android client) and notifies the UI only when something actually changed.
func addOrUpdateServer(info ServerInfo) {
	discoveryMu.Lock()
	prev, existed := discoveredServers[info.ServerName]
	discoveredServers[info.ServerName] = info
	discoveryMu.Unlock()

	if !existed || prev != info {
		log.Printf("discovery: server seen: %s (%s:%d)", info.ServerName, info.IP, info.Port)
		pushServerList()
	}
}

// snapshotServers returns the currently known servers, sorted by name for a
// stable UI order.
func snapshotServers() []ServerInfo {
	discoveryMu.Lock()
	defer discoveryMu.Unlock()

	servers := make([]ServerInfo, 0, len(discoveredServers))
	for _, s := range discoveredServers {
		servers = append(servers, s)
	}
	sort.Slice(servers, func(i, j int) bool { return servers[i].ServerName < servers[j].ServerName })
	return servers
}

// pushServerList sends the current discovery results to the UI.
func pushServerList() {
	data, err := json.Marshal(snapshotServers())
	if err != nil {
		log.Printf("discovery: marshal error: %v", err)
		return
	}
	if w == nil {
		return
	}
	w.Dispatch(func() {
		w.Eval(fmt.Sprintf("updateServerList(%s);", data))
	})
}
