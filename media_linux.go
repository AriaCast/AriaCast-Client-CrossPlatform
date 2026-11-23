//go:build linux

package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

// Linux: playback commands from the receiver are forwarded to the desktop's
// media player over MPRIS (D-Bus), much like the Android app forwards them
// to its media session. The captured audio keeps streaming either way.

const (
	mprisPrefix = "org.mpris.MediaPlayer2."
	mprisPath   = dbus.ObjectPath("/org/mpris/MediaPlayer2")
	mprisPlayer = "org.mpris.MediaPlayer2.Player"

	// mprisTimeout bounds each D-Bus call, so a hung player can't stall the
	// streaming loop that polls currentTrack.
	mprisTimeout = 500 * time.Millisecond
)

// forwardMediaCommand applies a /control action to the active MPRIS player.
// positionMs is only used by "seek".
func forwardMediaCommand(action string, positionMs int64) error {
	conn, err := dbus.SessionBus()
	if err != nil {
		return fmt.Errorf("session bus: %w", err)
	}

	name, _, err := activeMPRISPlayer(conn)
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
// playing, else the first one paused, else any MPRIS player at all. It also
// returns that player's org.mpris.MediaPlayer2.Player properties.
func activeMPRISPlayer(conn *dbus.Conn) (string, map[string]dbus.Variant, error) {
	ctx, cancel := context.WithTimeout(context.Background(), mprisTimeout)
	defer cancel()
	var names []string
	if err := conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.ListNames", 0).Store(&names); err != nil {
		return "", nil, fmt.Errorf("list bus names: %w", err)
	}

	var paused, other string
	var pausedProps, otherProps map[string]dbus.Variant
	for _, n := range names {
		if !strings.HasPrefix(n, mprisPrefix) {
			continue
		}
		props, err := playerProps(conn, n)
		if err != nil {
			continue
		}
		switch status, _ := props["PlaybackStatus"].Value().(string); status {
		case "Playing":
			return n, props, nil
		case "Paused":
			if paused == "" {
				paused, pausedProps = n, props
			}
		default:
			if other == "" {
				other, otherProps = n, props
			}
		}
	}
	if paused != "" {
		return paused, pausedProps, nil
	}
	if other != "" {
		return other, otherProps, nil
	}
	return "", nil, fmt.Errorf("no MPRIS media player found")
}

// playerProps fetches all org.mpris.MediaPlayer2.Player properties of a
// player in one call.
func playerProps(conn *dbus.Conn, name string) (map[string]dbus.Variant, error) {
	ctx, cancel := context.WithTimeout(context.Background(), mprisTimeout)
	defer cancel()
	var props map[string]dbus.Variant
	err := conn.Object(name, mprisPath).
		CallWithContext(ctx, "org.freedesktop.DBus.Properties.GetAll", 0, mprisPlayer).
		Store(&props)
	return props, err
}

// currentTrack reads the track of the active MPRIS player, or returns nil if
// there is no player.
func currentTrack() *trackInfo {
	conn, err := dbus.SessionBus()
	if err != nil {
		return nil
	}
	_, props, err := activeMPRISPlayer(conn)
	if err != nil {
		return nil
	}

	t := &trackInfo{PositionMs: -1}
	status, _ := props["PlaybackStatus"].Value().(string)
	t.Playing = status == "Playing"
	md, _ := props["Metadata"].Value().(map[string]dbus.Variant)
	t.Title, _ = md["xesam:title"].Value().(string)
	t.Album, _ = md["xesam:album"].Value().(string)
	t.ArtURL, _ = md["mpris:artUrl"].Value().(string)
	switch a := md["xesam:artist"].Value().(type) {
	case []string:
		t.Artist = strings.Join(a, ", ")
	case string:
		t.Artist = a
	}
	// MPRIS times are microseconds; players disagree on the integer type.
	if us, ok := variantInt(md["mpris:length"]); ok && us > 0 {
		t.DurationMs = us / 1000
	}
	if us, ok := variantInt(props["Position"]); ok && us >= 0 {
		t.PositionMs = us / 1000
	}
	return t
}

func variantInt(v dbus.Variant) (int64, bool) {
	switch n := v.Value().(type) {
	case int64:
		return n, true
	case uint64:
		return int64(n), true
	case int32:
		return int64(n), true
	case uint32:
		return int64(n), true
	}
	return 0, false
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
