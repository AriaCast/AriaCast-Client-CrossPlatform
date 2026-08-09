//go:build darwin

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

// Platform Specific - macOS: Requires separate audio capture driver like BlackHole for system audio
func getPlatformDeviceConfig() malgo.DeviceConfig {
	deviceConfig := malgo.DefaultDeviceConfig(malgo.Capture)
	deviceConfig.Capture.Format = AudioFormat
	deviceConfig.Capture.Channels = Channels
	deviceConfig.SampleRate = SampleRate
	deviceConfig.Alsa.NoMMap = 0 // Not relevant on Mac but kept for consistency
	return deviceConfig
}

// Window Methods specific to macOS
func (b *Binding) Minimize() {
	// macOS Appkit calls needed via CGO if we want programmatic minimize from JS
}

func (b *Binding) StartDrag() {
	// Handled by OS/Titlebar usually
}

func main() {
	debug := true
	w = webview.New(debug)
	defer w.Destroy()

	w.SetTitle("AriaCast - macOS Client")
	w.SetSize(450, 600, webview.HintNone)

	w.Bind("startStream", func() { (&Binding{}).Start() })
	w.Bind("stopStream", func() { (&Binding{}).Stop() })
	w.Bind("setVolume", func(v float64) { (&Binding{}).SetVolume(v) })
	w.Bind("refreshServers", func() { (&Binding{}).RefreshServers() })
	w.Bind("selectServer", func(name string) { (&Binding{}).SelectServer(name) })
	w.Bind("getServers", func() []ServerInfo { return (&Binding{}).GetServers() })

	htmlContent, err := uiFS.ReadFile("ui/index.html")
	if err != nil {
		log.Fatal(err)
	}
	w.SetHtml(string(htmlContent))

	go startDiscoveryLoop(context.Background())

	w.Run()
}
