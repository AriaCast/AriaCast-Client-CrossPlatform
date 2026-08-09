//go:build windows

package main

/*
#cgo LDFLAGS: -luser32 -lgdi32 -ldwmapi
#include <windows.h>
#include <dwmapi.h>

// Define constants for DWM in case headers are old
#ifndef DWMWA_WINDOW_CORNER_PREFERENCE
#define DWMWA_WINDOW_CORNER_PREFERENCE 33
#endif
#ifndef DWMWCP_ROUND
#define DWMWCP_ROUND 2
#endif

// Forward declaration
void setWindowRound(void* wnd);

void minimizeWindow(void* wnd) {
    ShowWindow((HWND)wnd, SW_MINIMIZE);
}

void removeBorders(void* wnd) {
    HWND hwnd = (HWND)wnd;
    LONG_PTR style = GetWindowLongPtr(hwnd, GWL_STYLE);
    style &= ~(WS_CAPTION | WS_THICKFRAME | WS_MINIMIZEBOX | WS_MAXIMIZEBOX | WS_SYSMENU);
    SetWindowLongPtr(hwnd, GWL_STYLE, style);
    SetWindowPos(hwnd, NULL, 0, 0, 0, 0,
                 SWP_FRAMECHANGED | SWP_NOMOVE | SWP_NOSIZE | SWP_NOZORDER | SWP_NOOWNERZORDER);

    // Apply rounding after removing borders
    setWindowRound(wnd);
}

void setWindowRound(void* wnd) {
    HWND hwnd = (HWND)wnd;
    // Attempt DWM rounding (Win 11) - makes system drawn shadow and smooth corners
    DWORD preference = DWMWCP_ROUND;
    DwmSetWindowAttribute(hwnd, DWMWA_WINDOW_CORNER_PREFERENCE, &preference, sizeof(preference));
}

void startDrag(void* wnd) {
    ReleaseCapture();
    SendMessage((HWND)wnd, WM_NCLBUTTONDOWN, HTCAPTION, 0);
}
*/
import "C"

import (
	"context"
	"embed"
	"log"

	"github.com/gen2brain/malgo"
	webview "github.com/webview/webview_go"
)

//go:embed ui/*
var uiFS embed.FS

// Platform Specific - Windows: Uses Loopback for capture
func getPlatformDeviceConfig() malgo.DeviceConfig {
	deviceConfig := malgo.DefaultDeviceConfig(malgo.Loopback)
	deviceConfig.Capture.Format = AudioFormat
	deviceConfig.Capture.Channels = Channels
	deviceConfig.SampleRate = SampleRate
	deviceConfig.Alsa.NoMMap = 1
	return deviceConfig
}

// Window Methods specific to Windows
func (b *Binding) Minimize() {
	if w != nil {
		w.Dispatch(func() {
			C.minimizeWindow(w.Window())
		})
	}
}

func (b *Binding) StartDrag() {
	if w != nil {
		w.Dispatch(func() {
			C.startDrag(w.Window())
		})
	}
}

func main() {
	debug := true
	w = webview.New(debug)
	defer w.Destroy()

	w.SetTitle("AriaCast - Modern Client")
	w.SetSize(450, 600, webview.HintFixed)

	w.Bind("startStream", func() { (&Binding{}).Start() })
	w.Bind("stopStream", func() { (&Binding{}).Stop() })
	w.Bind("setVolume", func(v float64) { (&Binding{}).SetVolume(v) })
	w.Bind("appClose", func() { (&Binding{}).Close() })
	w.Bind("appMinimize", func() { (&Binding{}).Minimize() })
	w.Bind("startDrag", func() { (&Binding{}).StartDrag() })
	w.Bind("refreshServers", func() { (&Binding{}).RefreshServers() })
	w.Bind("selectServer", func(name string) { (&Binding{}).SelectServer(name) })
	w.Bind("getServers", func() []ServerInfo { return (&Binding{}).GetServers() })

	w.Dispatch(func() {
		C.removeBorders(w.Window())
	})

	htmlContent, err := uiFS.ReadFile("ui/index.html")
	if err != nil {
		log.Fatal(err)
	}
	w.SetHtml(string(htmlContent))

	go startDiscoveryLoop(context.Background())

	w.Run()
}
