package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
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

func init() {
	logPath := "ariacast.log"
	exePath, exeErr := os.Executable()
	if exeErr == nil {
		logPath = filepath.Join(filepath.Dir(exePath), "ariacast.log")
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
	defer mu.Unlock()
	if isRunning && cancelFunc != nil {
		cancelFunc() // Cancel context
		isRunning = false
	}
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

	// 2. Connect WebSocket - FIXED PATH
	u := fmt.Sprintf("ws://%s:%d/audio", server.IP, server.Port)
	log.Printf("Connecting to %s", u)

	c, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		fail("Connection failed: " + err.Error())
		return
	}
	defer c.Close()

	log.Println("WebSocket connected")
	sendPlayingState(server, true)
	setStatus("Streaming Active")

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
				// We don't return here to keep capture unless fatal
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

	// Wait until context cancelled
	<-ctx.Done()
	log.Println("Context cancelled, stopping audio loop")
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

// scanOnce sends a single AriaCast discovery broadcast and collects replies
// for DiscoveryBurstWindow, pushing each newly-seen or updated server to the
// UI as it arrives.
func scanOnce(ctx context.Context) {
	pc, err := net.ListenPacket("udp4", ":0") // Bind to random port
	if err != nil {
		log.Printf("discovery: listen error: %v", err)
		return
	}
	defer pc.Close()

	broadcastAddr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("255.255.255.255:%d", DiscoveryPort))
	if err != nil {
		log.Printf("discovery: resolve error: %v", err)
		return
	}
	if _, err := pc.WriteTo([]byte(DiscoveryMsg), broadcastAddr); err != nil {
		log.Printf("discovery: send error: %v", err)
		return
	}
	log.Println("discovery: broadcast sent, listening for replies")

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
