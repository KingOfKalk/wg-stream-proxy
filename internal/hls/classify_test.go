// SPDX-License-Identifier: AGPL-3.0-or-later

package hls

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func TestClassify(t *testing.T) {
	errRead := errors.New("read failed")
	long := "<!--" + strings.Repeat("x", 1000) + "--><MPD>" + strings.Repeat(" ", 2000)
	tests := []struct {
		name        string
		contentType string
		body        string
		readErr     bool
		configured  Type
		sniff       bool
		want        Type
		refused     string
		err         error
	}{
		{"configured hls, body not read", "audio/mpeg", "[playlist]\n", false, HLS, true, HLS, "", nil},
		{"configured dash, body not read", "", "ID3", true, DASH, true, DASH, "", nil},
		{"hls content type, body not read", "application/x-mpegurl", "ID3", true, Auto, true, HLS, "", nil},
		{"dash content type, body not read", "application/dash+xml", "ID3", true, Auto, true, DASH, "", nil},
		{"hls body", "text/plain", "#EXTM3U\n", false, Auto, true, HLS, "", nil},
		{"dash body", "", "<MPD/>", false, Auto, true, DASH, "", nil},
		{"dash body after a long comment", "", long, false, Auto, true, DASH, "", nil},
		{"media", "video/mp2t", "\x47\x40\x00\x10", false, Auto, true, Raw, "", nil},
		{"pls refused", "", "[playlist]\nFile1=http://a.example/\n", false, Auto, true, Raw, FormatPLS, nil},
		{"pls content type refused", "audio/x-scpls", "\x47\x40\x00\x10", false, Auto, true, Raw, FormatPLS, nil},
		{"raw channel refuses dash", "", "<MPD/>", false, Raw, true, Raw, FormatDASH, nil},
		{"raw channel ignores the content type", "audio/x-scpls", "\x47\x40\x00\x10", false, Raw, true, Raw, "", nil},
		{"no sniffing, body not read", "", "[playlist]\n", true, Auto, false, Raw, "", nil},
		{"read error", "", "", true, Auto, true, Raw, "", errRead},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r io.Reader = strings.NewReader(tt.body)
			if tt.readErr {
				r = io.MultiReader(r, iotest.ErrReader(errRead))
			}
			body := bufio.NewReaderSize(r, SniffLen)
			typ, refused, rest, err := Classify(tt.contentType, body, tt.configured, tt.sniff)
			if typ != tt.want || refused != tt.refused || !errors.Is(err, tt.err) {
				t.Fatalf("Classify = %v, %q, %v; want %v, %q, %v", typ, refused, err, tt.want, tt.refused, tt.err)
			}
			if err != nil {
				return
			}
			// rest reads the whole body; a body that was not sniffed is
			// unread (it may fail later).
			b, err := io.ReadAll(rest)
			if tt.readErr {
				if !errors.Is(err, errRead) || string(b) != tt.body {
					t.Errorf("rest: %q, %v; want the unread body and its error", b, err)
				}
			} else if err != nil || string(b) != tt.body {
				t.Errorf("rest: %d bytes, %v; want the whole body", len(b), err)
			}
		})
	}
}
