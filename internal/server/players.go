// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
)

// The reload rule of error clips (spec 3.4): a player reloads the media
// playlist of a live channel every few seconds through /c/. A failure of
// such a reload gets the HTTP status, as with [clips] error = false: the
// VOD playlist of a clip would end playback, a status makes the player
// retry. Only a channel start gets the clip.

// reloadWindow is how long after a client got the media playlist of a
// channel from /c/ its next GET of the channel counts as a reload.
const reloadWindow = 30 * time.Second

// maxPlayers caps the remembered (channel, client) pairs; when full, the
// expired ones go, else the oldest.
const maxPlayers = 1024

// playerKey is a client playing a channel.
type playerKey struct {
	channel string
	client  netip.Addr
}

// player is when the client last got the channel's media playlist, and
// the channel as configured then.
type player struct {
	at time.Time
	ch config.Channel
}

// players remembers the clients that got the media playlist of a channel
// from /c/. It is safe for concurrent use. Entries expire lazily: no
// goroutine and no timer.
type players struct {
	now func() time.Time

	mu sync.Mutex
	m  map[playerKey]player
}

func newPlayers(now func() time.Time) *players {
	return &players{now: now, m: map[playerKey]player{}}
}

// clientAddr returns the address of the client of r without port; the
// zero Addr if it has none. Forwarded headers are not read (spec 7.1).
func clientAddr(r *http.Request) netip.Addr {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	return ap.Addr()
}

// served records that client got the media playlist of ch.
func (p *players) served(client netip.Addr, ch config.Channel) {
	if !client.IsValid() {
		return
	}
	k := playerKey{ch.ID(), client}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.m[k]; !ok && len(p.m) >= maxPlayers {
		p.evict(now)
	}
	p.m[k] = player{at: now, ch: ch}
}

// evict removes the expired entries or, if none expired, the oldest.
func (p *players) evict(now time.Time) {
	var oldest playerKey
	var at time.Time
	for k, e := range p.m {
		switch {
		case now.Sub(e.at) > reloadWindow:
			delete(p.m, k)
		case at.IsZero() || e.at.Before(at):
			oldest, at = k, e.at
		}
	}
	if len(p.m) >= maxPlayers {
		delete(p.m, oldest)
	}
}

// reload reports whether a GET of /c/ of ch by client is a reload: the
// client got the media playlist of the same, unchanged channel at most
// reloadWindow ago. A channel a config reload changed starts anew.
func (p *players) reload(client netip.Addr, ch config.Channel) bool {
	if !client.IsValid() {
		return false
	}
	k := playerKey{ch.ID(), client}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.m[k]
	switch {
	case !ok:
		return false
	case now.Sub(e.at) > reloadWindow:
		delete(p.m, k)
		return false
	}
	return e.ch.Equal(ch)
}
