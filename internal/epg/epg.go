// SPDX-License-Identifier: AGPL-3.0-or-later

package epg

import (
	"bufio"
	"compress/gzip"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/net/html/charset"
)

// MaxSize is the largest XMLTV file in bytes after decompression
// (spec 7.2). Real files are tens of MB.
const MaxSize = 200 << 20

// ErrTooLarge: the XMLTV file is larger than MaxSize after decompression.
var ErrTooLarge = errors.New("epg: file larger than 200 MiB")

// ErrNoChannels: the file has no <channel> element.
var ErrNoChannels = errors.New("epg: no channels found")

// Channel is a <channel> of the XMLTV file.
type Channel struct {
	ID    string
	Names []string // display names in file order, no duplicates
}

// Open returns the XMLTV bytes of r: gzip-compressed input (recognised by
// its magic bytes, not by name or header) is decompressed. Reading more
// than limit bytes of XML fails with ErrTooLarge.
func Open(r io.Reader, limit int64) (io.Reader, error) {
	br := bufio.NewReader(r)
	magic, err := br.Peek(2)
	if err != nil && err != io.EOF && !errors.Is(err, bufio.ErrBufferFull) {
		return nil, fmt.Errorf("epg: read: %w", err)
	}
	var src io.Reader = br
	if len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		zr, err := gzip.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("epg: gzip: %w", err)
		}
		src = zr
	}
	return &limitReader{r: src, n: limit}, nil
}

// limitReader returns ErrTooLarge instead of data past its limit.
type limitReader struct {
	r io.Reader
	n int64 // bytes still allowed
}

func (l *limitReader) Read(p []byte) (int, error) {
	if l.n < 0 {
		return 0, ErrTooLarge
	}
	// Read one byte more than allowed to tell "exactly at the limit" from "over".
	if int64(len(p)) > l.n+1 {
		p = p[:l.n+1]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	if l.n < 0 {
		return n + int(l.n), ErrTooLarge
	}
	return n, err
}

// Parse reads the channels of an XMLTV file with a streaming decoder; the
// file is never held in memory. By the XMLTV DTD all <channel> elements
// precede the <programme> elements, so parsing stops at the first
// <programme>. Channels with the same id are merged; channels without an
// id are skipped.
func Parse(r io.Reader) (*Index, error) {
	d := xml.NewDecoder(r)
	d.CharsetReader = charset.NewReaderLabel
	var chans []Channel
scan:
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if errors.Is(err, ErrTooLarge) {
				return nil, ErrTooLarge
			}
			return nil, fmt.Errorf("epg: invalid xmltv: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "programme":
			break scan
		case "channel":
			var c struct {
				ID    string   `xml:"id,attr"`
				Names []string `xml:"display-name"`
			}
			if err := d.DecodeElement(&c, &se); err != nil {
				if errors.Is(err, ErrTooLarge) {
					return nil, ErrTooLarge
				}
				return nil, fmt.Errorf("epg: invalid xmltv: %w", err)
			}
			if id := strings.TrimSpace(c.ID); id != "" {
				names := make([]string, 0, len(c.Names))
				for _, n := range c.Names {
					if n = strings.Join(strings.Fields(n), " "); n != "" {
						names = append(names, n)
					}
				}
				chans = append(chans, Channel{ID: id, Names: names})
			}
		}
	}
	if len(chans) == 0 {
		return nil, ErrNoChannels
	}
	return NewIndex(chans), nil
}
