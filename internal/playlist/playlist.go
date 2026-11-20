// SPDX-License-Identifier: AGPL-3.0-or-later

// Package playlist renders the channel list served as /playlist.m3u8
// (spec 6): an extended M3U with one #EXTINF entry and one absolute /c/
// URL per enabled channel.
package playlist

import (
	"net/url"
	"strings"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
)

// ContentType is the media type of the channel list (spec 6).
const ContentType = "audio/x-mpegurl; charset=utf-8"

// Options control Render.
type Options struct {
	// BaseURL is the absolute base of every URL in the list, without a
	// trailing slash, e.g. "http://wsp.lan:2349" (spec 7.1).
	BaseURL string
	// EPGURL is [epg] url; empty omits the EPG link from the header.
	EPGURL string
}

// Render returns the channel list for channels in their given order.
// Disabled channels are omitted. Lines end with LF; there is no BOM.
func Render(channels []config.Channel, opts Options) []byte {
	var b strings.Builder
	b.WriteString("#EXTM3U")
	if opts.EPGURL != "" {
		v := clean(opts.EPGURL)
		b.WriteString(` x-tvg-url="` + v + `" url-tvg="` + v + `"`)
	}
	b.WriteByte('\n')
	for _, ch := range channels {
		if !ch.Enabled() {
			continue
		}
		writeChannel(&b, ch, opts.BaseURL)
	}
	return []byte(b.String())
}

// writeChannel writes the extra lines, the #EXTINF line and the URL of
// one channel.
func writeChannel(b *strings.Builder, ch config.Channel, base string) {
	// Extra is sorted by directive, then key (config.Channel.Extra).
	for _, e := range ch.Extra() {
		b.WriteString("#" + e.Directive + ":" + e.Key + "=" + e.Value + "\n")
	}

	b.WriteString("#EXTINF:-1")
	attr := func(k, v string) {
		if v != "" {
			b.WriteString(" " + k + `="` + clean(v) + `"`)
		}
	}
	attr("tvg-id", ch.TVGID())
	attr("tvg-name", ch.Name())
	attr("tvg-logo", logoURL(ch.Logo(), base))
	attr("group-title", ch.Group())
	// Attrs is sorted by key (config.Channel.Attrs).
	for _, a := range ch.Attrs() {
		attr(a.Key, a.Value)
	}
	b.WriteString("," + clean(ch.Name()) + "\n")
	b.WriteString(base + ChannelPath(ch) + "\n")
}

// ChannelPath returns the list path of a channel (spec 6): /c/{id}.m3u8
// if its upstream type is hls, /c/{id}.mpd if it is dash; otherwise
// /c/{id}.m3u8 if its upstream URL path contains ".m3u8", /c/{id}.mpd if
// it contains ".mpd", else /c/{id}.
func ChannelPath(ch config.Channel) string {
	p := "/c/" + ch.ID()
	switch ch.Type() {
	case config.TypeHLS:
		return p + ".m3u8"
	case config.TypeDASH:
		return p + ".mpd"
	}
	u, err := url.Parse(ch.URL())
	switch {
	case err != nil:
	case strings.Contains(u.Path, ".m3u8"):
		return p + ".m3u8"
	case strings.Contains(u.Path, ".mpd"):
		return p + ".mpd"
	}
	return p
}

// logoURL returns the tvg-logo value: http(s) URLs and other values as
// configured, local files as {base}/logos/{file} (spec 5.4).
func logoURL(l config.Logo, base string) string {
	if l.Kind == config.LogoLocal {
		return base + "/logos/" + url.PathEscape(l.Value)
	}
	return l.Value
}

// clean replaces double quotes with single quotes so a value cannot end
// its attribute (spec 6). Line breaks are rejected by the config already.
func clean(s string) string {
	return strings.ReplaceAll(s, `"`, "'")
}
