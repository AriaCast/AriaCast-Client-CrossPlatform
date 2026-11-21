//go:build !windows && !linux

package webview

import (
	"image"
	"unsafe"
)

type icons struct{}

func (*icons) setIcon(_ unsafe.Pointer, _ image.Image, _ IconKind) {
}

func (*icons) free() {
}
