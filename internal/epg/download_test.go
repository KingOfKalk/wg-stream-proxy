// SPDX-License-Identifier: AGPL-3.0-or-later

package epg

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/route"
)

// directRoute returns the direct route with the loopback range allowed
// (the test server listens there) unless guard is true.
func directRoute(t *testing.T, guard bool, headerTimeout time.Duration) *route.Route {
	t.Helper()
	opts := route.Options{Upstream: config.Upstream{
		UserAgent:      "test",
		ConnectTimeout: 5 * time.Second,
		HeaderTimeout:  headerTimeout,
		ReadTimeout:    5 * time.Second,
	}}
	if !guard {
		opts.AllowPrivate = map[string][]netip.Prefix{config.RouteDirect: {netip.MustParsePrefix("127.0.0.0/8")}}
	}
	set, err := route.NewSet(route.Host(), nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(set.CloseIdleConnections)
	rt, _ := set.Route(config.RouteDirect)
	return rt
}

func TestDownload(t *testing.T) {
	plain := fixture(t)
	zipped := gz(t, plain)
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/plain.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write(plain)
	})
	mux.HandleFunc("/epg.xml.gz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(zipped)
	})
	mux.HandleFunc("/octet", func(w http.ResponseWriter, r *http.Request) {
		// Compressed data under a plain name and type: only the magic bytes tell.
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(zipped)
	})
	mux.HandleFunc("/encoded.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(zipped)
	})
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/plain.xml", http.StatusFound)
	})
	mux.HandleFunc("/missing", http.NotFound)
	mux.HandleFunc("/html", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html><body>maintenance</body></html>"))
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	defer close(release)

	tests := []struct {
		name    string
		path    string
		guard   bool
		wantErr string
	}{
		{name: "plain", path: "/plain.xml"},
		{name: "gzip by magic bytes", path: "/epg.xml.gz"},
		{name: "gzip without hint", path: "/octet"},
		{name: "content-encoding gzip", path: "/encoded.xml"},
		{name: "redirect", path: "/redirect"},
		{name: "not found", path: "/missing", wantErr: "status 404"},
		{name: "not xmltv", path: "/html", wantErr: "no channels"},
		{name: "guard blocks loopback", path: "/plain.xml", guard: true, wantErr: "blocked"},
		{name: "header timeout", path: "/slow", wantErr: "timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := directRoute(t, tt.guard, 300*time.Millisecond)
			x, err := Download(context.Background(), rt, srv.URL+tt.path)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := x.Channels(); !reflect.DeepEqual(got, fixtureChannels) {
				t.Errorf("channels = %#v", got)
			}
		})
	}
}

func TestDownloadGuardReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	_, err := Download(context.Background(), directRoute(t, true, time.Second), srv.URL)
	if got := route.Classify(err); got != route.ReasonGuard {
		t.Errorf("reason = %q (%v), want guard", got, err)
	}
}

func TestDownloadCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	if _, err := Download(ctx, directRoute(t, false, time.Second), srv.URL); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestParseBodyLimit(t *testing.T) {
	_, err := parseBody(strings.NewReader("<tv>"+strings.Repeat(" ", 100)+"</tv>"), 50)
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("err = %v, want ErrTooLarge", err)
	}
}
