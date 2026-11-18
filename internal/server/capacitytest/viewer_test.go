// SPDX-License-Identifier: AGPL-3.0-or-later

package capacitytest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// requestTimeout bounds every request of a viewer, so a hanging
	// request fails the test instead of blocking it.
	requestTimeout = 10 * time.Second
	// maxErrors is the number of errors a viewer keeps; it counts all.
	maxErrors = 10
)

// viewer plays one channel like an HLS player at the live edge: it loads
// the master playlist, then polls the media playlist and fetches every
// new segment in order. It checks that no segment is skipped, that each
// has the fixture's content, and that each arrives within stall of being
// published. A viewer is used by one goroutine.
type viewer struct {
	ch     string // channel name at the fixture: the segment content
	url    string // channel URL at the proxy
	client *http.Client
	live   *live
	stall  time.Duration
	now    func() time.Time

	media      string // media playlist URL, from the master playlist
	first      int    // first segment fetched, -1 before
	last       int    // last segment fetched, -1 before
	segments   int    // segments fetched
	maxLatency time.Duration
	errs       []error
	failures   int // all errors, also those not kept in errs
}

func newViewer(ch, url string, client *http.Client, l *live, stall time.Duration) *viewer {
	return &viewer{ch: ch, url: url, client: client, live: l, stall: stall, now: time.Now, first: -1, last: -1}
}

// start loads the master playlist and picks its first variant.
func (v *viewer) start(ctx context.Context) error {
	body, base, err := v.get(ctx, v.url)
	if err != nil {
		return err
	}
	uris := uriLines(string(body))
	if len(uris) == 0 {
		return fmt.Errorf("%s: no variant in the master playlist", v.ch)
	}
	ref, err := base.Parse(uris[0])
	if err != nil {
		return fmt.Errorf("%s: variant: %w", v.ch, err)
	}
	v.media = ref.String()
	return nil
}

// run polls every poll until ctx ends.
func (v *viewer) run(ctx context.Context, poll time.Duration) {
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		v.step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// step loads the media playlist once and fetches its new segments. The
// first step starts at the newest segment, like a player at the live
// edge.
func (v *viewer) step(ctx context.Context) {
	body, base, err := v.get(ctx, v.media)
	if err != nil {
		v.fail(ctx, err)
		return
	}
	seq0, uris, err := parseMedia(string(body))
	if err != nil {
		v.fail(ctx, fmt.Errorf("media playlist: %w", err))
		return
	}
	if v.last < 0 {
		v.last = seq0 + len(uris) - 2
	}
	for i, u := range uris {
		seq := seq0 + i
		if seq <= v.last {
			continue
		}
		if seq != v.last+1 {
			v.fail(ctx, fmt.Errorf("segments %d-%d left the window before they were fetched", v.last+1, seq-1))
		}
		ref, err := base.Parse(u)
		if err != nil {
			v.fail(ctx, fmt.Errorf("segment %d: %w", seq, err))
			return
		}
		got, _, err := v.get(ctx, ref.String())
		if err != nil {
			v.fail(ctx, fmt.Errorf("segment %d: %w", seq, err))
			return
		}
		if !bytes.Equal(got, segment(v.ch, seq, v.live.size)) {
			v.fail(ctx, fmt.Errorf("segment %d: wrong content (%d bytes)", seq, len(got)))
		}
		if at, ok := v.live.publishedAt(seq); ok {
			lat := v.now().Sub(at)
			v.maxLatency = max(v.maxLatency, lat)
			if lat > v.stall {
				v.fail(ctx, fmt.Errorf("segment %d: stall: arrived %s after it was published (bound %s)", seq, lat, v.stall))
			}
		}
		if v.first < 0 {
			v.first = seq
		}
		v.last = seq
		v.segments++
	}
}

// get fetches u and returns its body and final URL.
func (v *viewer) get(ctx context.Context, u string) ([]byte, *url.URL, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return body, resp.Request.URL, nil
}

// fail records err unless ctx ended: requests cut off by the end of the
// run are not failures.
func (v *viewer) fail(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return
	}
	v.failures++
	if len(v.errs) < maxErrors {
		v.errs = append(v.errs, fmt.Errorf("%s: %w", v.ch, err))
	}
}

// errors returns the recorded errors.
func (v *viewer) errors() []error { return v.errs }

// uriLines returns the URI lines of a playlist.
func uriLines(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

// parseMedia returns the media sequence number and the segment URIs of a
// media playlist.
func parseMedia(body string) (int, []string, error) {
	const tag = "#EXT-X-MEDIA-SEQUENCE:"
	for _, line := range strings.Split(body, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), tag); ok {
			seq, err := strconv.Atoi(v)
			if err != nil {
				return 0, nil, err
			}
			return seq, uriLines(body), nil
		}
	}
	return 0, nil, errors.New("no " + strings.TrimSuffix(tag, ":"))
}
