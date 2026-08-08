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
	w          webview.WebView
	mu         sync.Mutex
	statusMsg  string  = "Idle"
	serverInfo string  = "Not Connected"
	volume     float64 = 1.0
	isRunning  bool
	cancelFunc context.CancelFunc
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
			// Simple escape for single quotes
			// full json encoding would be safer but this is lightweight
			js := fmt.Sprintf("updateStatus('%s');", msg)
			w.Eval(js)
		})
	}
}

func setServerInfo(msg string) {
	mu.Lock()
	serverInfo = msg
	mu.Unlock()

	if w != nil {
		w.Dispatch(func() {
			js := fmt.Sprintf("updateServerInfo('%s');", msg)
			w.Eval(js)
		})
	}
}

// -----------------------------------------------------------------------------
// Audio Logic (Common)
// -----------------------------------------------------------------------------

const (
	DiscoveryPort     = 12888
	DefaultServerPort = 12889
	DiscoveryTimeout  = 2 * time.Second
	DiscoveryMsg      = "DISCOVER_AUDIOCAST"
	SampleRate        = 48000
	Channels          = 2
	AudioFormat       = malgo.FormatS16
	ProtocolFrameSize = 3840
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

	// 1. Discovery (or manual override from ariacast_config.json)
	var server *ServerInfo
	if cfg := loadManualConfig(); cfg != nil {
		log.Printf("Using manual server config: %s (%s:%d)", cfg.ServerName, cfg.IP, cfg.Port)
		setStatus("Using configured server...")
		server = cfg
	} else {
		log.Println("Scanning for server...")
		setStatus("Scanning for server...")
		var err error
		server, err = discoverServer(ctx)
		if err != nil {
			fail("Error: " + err.Error())
			return
		}
		log.Printf("Server found: %s (%s:%d)", server.ServerName, server.IP, server.Port)
	}
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

func discoverServer(ctx context.Context) (*ServerInfo, error) {
	pc, err := net.ListenPacket("udp4", ":0") // Bind to random port
	if err != nil {
		return nil, err
	}
	defer pc.Close()

	addr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("255.255.255.255:%d", DiscoveryPort))
	if err != nil {
		return nil, err
	}

	// Send discovery packet
	if _, err := pc.WriteTo([]byte(DiscoveryMsg), addr); err != nil {
		return nil, err
	}

	// Read loop
	buf := make([]byte, 1024)
	resultChan := make(chan *ServerInfo, 1)

	go func() {
		pc.SetReadDeadline(time.Now().Add(DiscoveryTimeout))
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}

		var info ServerInfo
		if err := json.Unmarshal(buf[:n], &info); err == nil {
			resultChan <- &info
		}
	}()

	select {
	case info := <-resultChan:
		return info, nil
	case <-time.After(DiscoveryTimeout):
		return nil, fmt.Errorf("timeout looking for server")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
