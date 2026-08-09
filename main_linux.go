//go:build linux

package main

import (
	"context"
	"embed"
	"log"

	"github.com/gen2brain/malgo"
	webview "github.com/webview/webview_go"
)

//go:embed ui/*
var uiFS embed.FS

// Platform Specific - Linux: Typically use PulseAudio Monitor for desktop audio
func getPlatformDeviceConfig() malgo.DeviceConfig {
	// On Linux, to capture "what you hear", you usually need to select the monitor source of the output.
	// Malgo/MiniAudio might auto-detect if we ask for Loopback, or we might need to pick the default input.
	// For simplicity in this cross-platform example, we will try the default capture device.
	// Advanced Linux users would use pavucontrol to redirect the monitor to this app's input.
	deviceConfig := malgo.DefaultDeviceConfig(malgo.Capture)
	deviceConfig.Capture.Format = AudioFormat
	deviceConfig.Capture.Channels = Channels
	deviceConfig.SampleRate = SampleRate
	deviceConfig.Alsa.NoMMap = 1
	return deviceConfig
}

// Window Methods specific to Linux (No-op or different implementation as needed)
func (b *Binding) Minimize() {
	// Linux webview implementation limitations might prevent direct minimize control easily without CGO/GTK calls
}

func (b *Binding) StartDrag() {
	// Linux window dragging - usually handled by Window Manager if decorated
}

func main() {
	debug := true
	w = webview.New(debug)
	defer w.Destroy()

	w.SetTitle("AriaCast - Linux Client")
	w.SetSize(450, 600, webview.HintNone)

	// Bindings
	w.Bind("startStream", func() { (&Binding{}).Start() })
	w.Bind("stopStream", func() { (&Binding{}).Stop() })
	w.Bind("setVolume", func(v float64) { (&Binding{}).SetVolume(v) })
	w.Bind("refreshServers", func() { (&Binding{}).RefreshServers() })
	w.Bind("selectServer", func(name string) { (&Binding{}).SelectServer(name) })
	w.Bind("getServers", func() []ServerInfo { return (&Binding{}).GetServers() })

	// Linux might not support borderless dragging easily in webview without more complex GTK logic
	// So we keep standard decorations (HintNone) or use HintFixed if we want non-resizable.

	htmlContent, err := uiFS.ReadFile("ui/index.html")
	if err != nil {
		log.Fatal(err)
	}
	w.SetHtml(string(htmlContent))

	go startDiscoveryLoop(context.Background())

	w.Run()
}
