//go:build linux

package main

import (
	"fmt"
	"log"
	"strings"

	"github.com/godbus/dbus/v5"
)

// Linux: playback commands from the receiver are forwarded to the desktop's
// media player over MPRIS (D-Bus), much like the Android app forwards them
// to its media session. The captured audio keeps streaming either way.

const (
	mprisPrefix = "org.mpris.MediaPlayer2."
	mprisPath   = dbus.ObjectPath("/org/mpris/MediaPlayer2")
	mprisPlayer = "org.mpris.MediaPlayer2.Player"
)

// forwardMediaCommand applies a /control action to the active MPRIS player.
// positionMs is only used by "seek".
func forwardMediaCommand(action string, positionMs int64) error {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return fmt.Errorf("session bus: %w", err)
	}
	defer conn.Close()

	name, err := activeMPRISPlayer(conn)
	if err != nil {
		return err
	}
	player := conn.Object(name, mprisPath)

	var method string
	switch action {
	case "play":
		method = "Play"
	case "pause":
		method = "Pause"
	case "toggle", "play_pause":
		method = "PlayPause"
	case "next":
		method = "Next"
	case "previous":
		method = "Previous"
	case "stop":
		method = "Stop"
	case "seek":
		return mprisSeek(player, positionMs)
	default:
		return fmt.Errorf("unsupported action %q", action)
	}
	log.Printf("MPRIS: %s.%s on %s", mprisPlayer, method, name)
	return player.Call(mprisPlayer+"."+method, 0).Err
}

// activeMPRISPlayer picks the player to control: the first one currently
// playing, else the first one paused, else any MPRIS player at all.
func activeMPRISPlayer(conn *dbus.Conn) (string, error) {
	var names []string
	if err := conn.BusObject().Call("org.freedesktop.DBus.ListNames", 0).Store(&names); err != nil {
		return "", fmt.Errorf("list bus names: %w", err)
	}

	var paused, other string
	for _, n := range names {
		if !strings.HasPrefix(n, mprisPrefix) {
			continue
		}
		v, err := conn.Object(n, mprisPath).GetProperty(mprisPlayer + ".PlaybackStatus")
		if err != nil {
			continue
		}
		switch status, _ := v.Value().(string); status {
		case "Playing":
			return n, nil
		case "Paused":
			if paused == "" {
				paused = n
			}
		default:
			if other == "" {
				other = n
			}
		}
	}
	if paused != "" {
		return paused, nil
	}
	if other != "" {
		return other, nil
	}
	return "", fmt.Errorf("no MPRIS media player found")
}

// mprisSeek jumps to an absolute position. MPRIS SetPosition needs the
// current track's mpris:trackid; players that don't expose one get a
// relative Seek from the current Position instead.
func mprisSeek(player dbus.BusObject, positionMs int64) error {
	if positionMs < 0 {
		positionMs = 0
	}
	target := positionMs * 1000 // MPRIS positions are in microseconds

	if v, err := player.GetProperty(mprisPlayer + ".Metadata"); err == nil {
		if md, ok := v.Value().(map[string]dbus.Variant); ok {
			if id, ok := md["mpris:trackid"].Value().(dbus.ObjectPath); ok && id.IsValid() {
				log.Printf("MPRIS: SetPosition(%s, %dus)", id, target)
				return player.Call(mprisPlayer+".SetPosition", 0, id, target).Err
			}
		}
	}

	v, err := player.GetProperty(mprisPlayer + ".Position")
	if err != nil {
		return fmt.Errorf("read position: %w", err)
	}
	current, ok := v.Value().(int64)
	if !ok {
		return fmt.Errorf("unexpected Position type %T", v.Value())
	}
	log.Printf("MPRIS: Seek(%dus) from %dus", target-current, current)
	return player.Call(mprisPlayer+".Seek", 0, target-current).Err
}
