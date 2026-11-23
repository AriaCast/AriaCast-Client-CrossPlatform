//go:build !linux

package main

import "fmt"

// forwardMediaCommand is not implemented outside Linux yet; Windows (SMTC)
// and macOS have no simple public API for controlling another app's player.
func forwardMediaCommand(action string, positionMs int64) error {
	return fmt.Errorf("media control not supported on this platform")
}

// currentTrack is not implemented outside Linux yet; the receiver gets the
// generic "AriaCast Desktop" metadata instead.
func currentTrack() *trackInfo {
	return nil
}
