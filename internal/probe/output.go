// SPDX-License-Identifier: AGPL-3.0-or-later

package probe

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// WriteJSON writes the report as one indented JSON object.
func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// roleTitles are the titles of the steps in the text form.
var roleTitles = map[string]string{
	RoleURL:     "url",
	RoleVariant: "variant playlist",
	RoleSegment: "first segment",
}

// WriteText writes the report for a person: per step the URL in its
// logged form, status, content type, detected format, size and timing,
// and a last line with the outcome.
func (r *Report) WriteText(w io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "route %s%s\n", r.Route, r.modeNote())
	for _, s := range r.Steps {
		fmt.Fprintf(&b, "%s: %s\n", roleTitles[s.Role], s.URL)
		field := func(name, value string) { fmt.Fprintf(&b, "  %-13s %s\n", name, value) }
		if s.Status != 0 {
			field("status", fmt.Sprint(s.Status))
		}
		if s.ContentType != "" {
			field("content-type", s.ContentType)
		}
		if s.Redirects > 0 {
			field("redirects", fmt.Sprint(s.Redirects))
		}
		if f := s.describeFormat(); f != "" {
			field("format", f)
		}
		switch {
		case s.Size != nil:
			field("size", fmt.Sprintf("%d bytes", *s.Size))
		case s.Streaming:
			field("size", "streaming")
		}
		if t := s.Timing.describe(); t != "" {
			field("timing", t)
		}
		if !s.OK {
			field("error", s.Class+": "+s.Error)
		}
	}
	if r.OK {
		b.WriteString("result: ok, the proxy would serve this URL\n")
	} else {
		b.WriteString("result: failed, the proxy would not serve this URL\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// describeFormat returns the format of a step in words.
func (s Step) describeFormat() string {
	switch s.Format {
	case FormatHLSMaster:
		return fmt.Sprintf("HLS master playlist, %s", plural(s.Variants, "variant"))
	case FormatHLSMedia:
		return fmt.Sprintf("HLS media playlist, %s", plural(s.Segments, "segment"))
	case FormatRaw:
		return "raw stream"
	case FormatICY:
		return "raw stream (ICY)"
	case FormatDASH:
		return fmt.Sprintf("DASH manifest, %s, %s", plural(s.Periods, "period"), plural(s.AdaptationSets, "adaptation set"))
	case FormatRefused:
		return "refused: " + s.Refused + " playlist"
	}
	return s.Format
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// describe lists the phases that happened.
func (t Timing) describe() string {
	var parts []string
	add := func(name string, v *float64) {
		if v != nil {
			parts = append(parts, fmt.Sprintf("%s %.1f ms", name, *v))
		}
	}
	add("tunnel start", t.TunnelStartMS)
	add("dns", t.DNSMS)
	add("connect", t.ConnectMS)
	add("tls", t.TLSMS)
	add("first byte", t.TTFBMS)
	parts = append(parts, fmt.Sprintf("total %.1f ms", t.TotalMS))
	out := strings.Join(parts, ", ")
	if t.ConnReused {
		out += " (connection reused)"
	}
	return out
}

// modeNote returns the mode for the first line of the text form.
func (r *Report) modeNote() string {
	switch r.Mode {
	case ModeProxy:
		return " (via running proxy)"
	case ModeStandalone:
		return " (standalone)"
	}
	return ""
}
