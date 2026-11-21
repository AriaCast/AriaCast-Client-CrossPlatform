module ariacast_client

go 1.25.5

require (
	github.com/gen2brain/malgo v0.11.24
	github.com/gorilla/websocket v1.5.3
	github.com/webview/webview_go v0.0.0-20240831120633-6173450d4dd6
)

// Vendored locally and patched to link against webkit2gtk-4.1 instead of the
// upstream-hardcoded webkit2gtk-4.0, which no longer exists on Ubuntu 24.04+
// and other current distros. See third_party/webview_go/webview.go.
replace github.com/webview/webview_go => ./third_party/webview_go
