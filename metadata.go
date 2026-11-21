package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// trackInfo describes what the desktop is playing, as reported by the
// platform's media session (MPRIS on Linux).
type trackInfo struct {
	Title, Artist, Album string
	DurationMs           int64  // 0 = unknown
	PositionMs           int64  // -1 = unknown
	ArtURL               string // as given by the player (file://, http(s)://, ...)
	Playing              bool
}

// metadataRefresh is how often unchanged metadata is re-sent anyway, so the
// receiver recovers it after a restart (spec metadata.md, "Periodic
// metadata refresh").
const metadataRefresh = 10 * time.Second

var metadataClient = &http.Client{Timeout: 3 * time.Second}

// metadataReporter keeps the receiver's /metadata in sync with the desktop
// player, like the Android app does with its media session.
type metadataReporter struct {
	server   *ServerInfo
	lastKey  string
	lastSent time.Time
}

// report POSTs the current track when it changed, every call while the player
// is playing (position tracking; called once a second), and at least every
// metadataRefresh otherwise. force sends regardless, e.g. after a (re)connect.
func (r *metadataReporter) report(force bool) {
	t := currentTrack()
	key := ""
	if t != nil {
		key = fmt.Sprintf("%s|%s|%s|%d|%s|%v", t.Title, t.Artist, t.Album, t.DurationMs, t.ArtURL, t.Playing)
	}
	changed := key != r.lastKey
	if !force && !changed && (t == nil || !t.Playing) && time.Since(r.lastSent) < metadataRefresh {
		return
	}
	r.lastKey = key
	r.lastSent = time.Now()
	// Position-only updates go out every second; only log real changes.
	sendMetadata(r.server, true, t, force || changed)
}

// sendMetadata mirrors the AriaCast Android app's metadata POST
// (see AudioCastService.performMetadataUpdate / TrackMetadata). "isPlaying"
// tells the receiver a streaming session is active, so it follows the
// stream, not the player. Without a track the generic "AriaCast Desktop"
// title is sent.
func sendMetadata(server *ServerInfo, playing bool, t *trackInfo, logIt bool) {
	data := map[string]interface{}{
		"title":      "AriaCast Desktop",
		"artist":     nil,
		"album":      nil,
		"artworkUrl": nil,
		"durationMs": nil,
		"positionMs": nil,
		"isPlaying":  playing,
	}
	if t != nil {
		if t.Title != "" {
			data["title"] = t.Title
		}
		if t.Artist != "" {
			data["artist"] = t.Artist
		}
		if t.Album != "" {
			data["album"] = t.Album
		}
		if t.DurationMs > 0 {
			data["durationMs"] = t.DurationMs
		}
		if t.PositionMs >= 0 {
			data["positionMs"] = t.PositionMs
		}
		// Only a web URL is usable by a receiver on another machine; a
		// player's local file:// cover is left out.
		if strings.HasPrefix(t.ArtURL, "http://") || strings.HasPrefix(t.ArtURL, "https://") {
			data["artworkUrl"] = t.ArtURL
		}
	}

	url := fmt.Sprintf("http://%s:%d/metadata", server.IP, server.Port)
	body, _ := json.Marshal(map[string]interface{}{"data": data})
	resp, err := metadataClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("Metadata POST to %s failed: %v", url, err)
		return
	}
	defer resp.Body.Close()
	if logIt || resp.StatusCode != http.StatusOK {
		log.Printf("Metadata POST to %s: %s %s", url, body, resp.Status)
	}
}
