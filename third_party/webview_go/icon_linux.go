//go:build linux

package webview

/*
#cgo pkg-config: gtk+-3.0
#include <gtk/gtk.h>
#include <gdk-pixbuf/gdk-pixbuf.h>

static void webviewSetWindowIcon(void* wnd, const char* data, int n) {
	GInputStream* stream = g_memory_input_stream_new_from_data(
		(const void*)data, (gssize)n, NULL);
	GError* err = NULL;
	GdkPixbuf* pixbuf = gdk_pixbuf_new_from_stream(stream, NULL, &err);
	g_object_unref(stream);
	if (err) { g_error_free(err); return; }
	if (pixbuf) {
		gtk_window_set_icon(GTK_WINDOW(wnd), pixbuf);
		g_object_unref(pixbuf);
	}
}
*/
import "C"

import (
	"bytes"
	"image"
	"image/png"
	"unsafe"
)

type icons struct{}

// setIcon converts the image.Image to a PNG in memory and loads it into the
// GtkWindow via GdkPixbuf. The kind parameter is ignored on GTK (there is no
// separate "small" icon slot; GTK/the WM scales the single icon itself).
func (*icons) setIcon(window unsafe.Pointer, icon image.Image, _ IconKind) {
	if window == nil || icon == nil {
		return
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, icon); err != nil {
		return
	}
	b := buf.Bytes()
	C.webviewSetWindowIcon(window, (*C.char)(unsafe.Pointer(&b[0])), C.int(len(b)))
}

func (*icons) free() {}
