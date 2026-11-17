// SPDX-License-Identifier: AGPL-3.0-or-later

package capacitytest

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/hls"
	"github.com/KingOfKalk/wg-stream-proxy/internal/server"
	"github.com/KingOfKalk/wg-stream-proxy/internal/tunnel/tunneltest"
)

const (
	// envDuration enables the capacity test and sets its duration: a Go
	// duration ("30s", "5m"), or a true boolean ("1") for
	// defaultDuration. Unset or false skips the test.
	envDuration = "WSP_CAPACITY"
	// defaultDuration is the duration on push (testing.md L3).
	defaultDuration = 30 * time.Second
	// minDuration leaves room for the warm-up and a few samples.
	minDuration = 15 * time.Second

	// segDur is the segment duration of the live fixture; a new segment
	// is published every segDur.
	segDur = time.Second
	// window is the number of segments in the live media playlist.
	window = 6
	// segSize is the size of a segment: 1 Mbit/s per viewer. The test
	// checks stability over time, not throughput.
	segSize = 128 << 10
	// pollEvery is how often a viewer loads the media playlist.
	pollEvery = segDur / 2
	// stallBound is the longest a segment may take from publishing to
	// arriving at the viewer: three target durations, the buffer a
	// player starts with.
	stallBound = 3 * segDur

	// maxWarmup is the time after which goroutines and heap must stay
	// flat: tunnels are up and connections pooled.
	maxWarmup = 10 * time.Second
	// maxSampleEvery is the longest interval between two samples.
	maxSampleEvery = 5 * time.Second
	// goroutineSlack is the goroutine growth over the baseline that is
	// still in-flight requests rather than a leak. A leak of one
	// goroutine per request exceeds it within seconds.
	goroutineSlack = 20
	// heapSlack is the heap growth over the baseline (after GC) that is
	// still noise rather than a leak.
	heapSlack = 32 << 20
)

// testKey is a test-only signing key.
var testKey = []byte("test-only-signing-key-0123456789")

// parseDuration parses the value of envDuration. run is false if the
// test is to be skipped.
func parseDuration(v string) (d time.Duration, run bool, err error) {
	if v == "" {
		return 0, false, nil
	}
	if on, err := strconv.ParseBool(v); err == nil {
		if !on {
			return 0, false, nil
		}
		return defaultDuration, true, nil
	}
	d, err = time.ParseDuration(v)
	if err != nil {
		return 0, false, fmt.Errorf("%s=%q: %w", envDuration, v, err)
	}
	if d < minDuration {
		return 0, false, fmt.Errorf("%s=%q: shorter than %s", envDuration, v, minDuration)
	}
	return d, true, nil
}

// sample is a measurement during the run.
type sample struct {
	at         time.Duration // since the viewers started
	goroutines int
	heap       uint64 // runtime.MemStats.HeapInuse after GC
}

func takeSample(start time.Time) sample {
	// Two cycles empty the sync.Pools (wireguard-go's message buffers):
	// the first moves them to the victim cache, the second frees them.
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return sample{at: time.Since(start), goroutines: runtime.NumGoroutine(), heap: m.HeapInuse}
}

// checkGrowth compares every sample after warmup with the first one
// after warmup (the baseline) and returns the growth beyond the slack.
func checkGrowth(samples []sample, warmup time.Duration) []string {
	var problems []string
	var base *sample
	for i := range samples {
		s := samples[i]
		if s.at < warmup {
			continue
		}
		if base == nil {
			base = &samples[i]
			continue
		}
		if s.goroutines > base.goroutines+goroutineSlack {
			problems = append(problems, fmt.Sprintf("goroutines grew from %d to %d at %s", base.goroutines, s.goroutines, s.at))
		}
		if s.heap > base.heap+heapSlack {
			problems = append(problems, fmt.Sprintf("heap in use grew from %.1f MiB to %.1f MiB at %s", mib(base.heap), mib(s.heap), s.at))
		}
	}
	if base == nil {
		problems = append(problems, "no sample after the warm-up")
	}
	return problems
}

func mib(b uint64) float64 { return float64(b) / (1 << 20) }

// TestCapacity is the capacity test of testing.md (L3): three viewers on
// three channels through server.Run, two channels sharing one in-process
// tunnel and the third on a second one, for the duration set with
// WSP_CAPACITY. It checks that every viewer gets every segment, in order,
// unaltered and within stallBound, and that goroutines and heap do not
// grow while the viewers keep watching. Wall-clock time is the point of
// this test, so it uses real tickers instead of an injected clock.
func TestCapacity(t *testing.T) {
	d, run, err := parseDuration(os.Getenv(envDuration))
	if err != nil {
		t.Fatal(err)
	}
	if !run {
		t.Skipf("set %s=30s (or another duration, at least %s) to run the capacity test", envDuration, minDuration)
	}
	// Goroutines of the test runner; everything started here must be
	// gone at the end.
	baseline := goleak.IgnoreCurrent()

	fx := newLive(segDur, window, segSize, time.Now)
	for range window {
		fx.advance()
	}
	peerA := tunneltest.Start(t, tunneltest.Options{Handler: fx})
	peerA.SetHost("a.test", tunneltest.ServerAddr4)
	peerB := tunneltest.Start(t, tunneltest.Options{Handler: fx})
	peerB.SetHost("b.test", tunneltest.ServerAddr4)

	dir := writeConfig(t, map[string]string{
		"config.toml":    "",
		"tunnels/a.conf": peerA.ClientConfig(),
		"tunnels/b.conf": peerB.ClientConfig(),
		// The route is the file stem (spec 5.2).
		"channels.d/a.toml": `
[[channel]]
name = "One"
url = "http://a.test/one/master.m3u8"

[[channel]]
name = "Two"
url = "http://a.test/two/master.m3u8"
`,
		"channels.d/b.toml": `
[[channel]]
name = "Three"
url = "http://b.test/three/master.m3u8"
`,
	})
	cfg, err := config.Load(config.Options{Paths: config.Paths{ConfigDir: dir, TunnelsDir: filepath.Join(dir, "tunnels")}})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	signer, err := hls.NewSigner(testKey)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxyURL := "http://" + ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- server.Run(ctx, server.RunOptions{
			Config:          cfg,
			Signer:          signer,
			Logger:          slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelWarn})),
			Version:         "capacity",
			Listener:        ln,
			ShutdownTimeout: time.Second,
		})
	}()
	var stopOnce sync.Once
	stopProxy := func() {
		stopOnce.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("Run: %v", err)
			}
		})
	}
	t.Cleanup(stopProxy)

	// Each viewer has its own client, like separate players.
	var viewers []*viewer
	vctx, vcancel := context.WithCancel(context.Background())
	defer vcancel()
	for _, ch := range []string{"one", "two", "three"} {
		client := &http.Client{Transport: &http.Transport{}}
		t.Cleanup(client.CloseIdleConnections)
		v := newViewer(ch, proxyURL+"/c/"+ch+".m3u8", client, fx, stallBound)
		if err := v.start(vctx); err != nil {
			t.Fatalf("viewer %s: %v", ch, err)
		}
		viewers = append(viewers, v)
	}
	start := time.Now()
	var wg sync.WaitGroup
	for _, v := range viewers {
		wg.Go(func() { v.run(vctx, pollEvery) })
	}

	// The test drives the live edge and samples goroutines and heap.
	warmup := min(maxWarmup, d/3)
	sampleEvery := min(maxSampleEvery, d/6)
	publish := time.NewTicker(segDur)
	defer publish.Stop()
	measure := time.NewTicker(sampleEvery)
	defer measure.Stop()
	end := time.NewTimer(d)
	defer end.Stop()
	firstSeq := fx.newest()
	var samples []sample
loop:
	for {
		select {
		case <-publish.C:
			fx.advance()
		case <-measure.C:
			samples = append(samples, takeSample(start))
		case <-end.C:
			break loop
		}
	}
	samples = append(samples, takeSample(start))
	newest := fx.newest()

	vcancel()
	wg.Wait()
	for _, v := range viewers {
		v.client.CloseIdleConnections()
	}
	stopProxy()
	peerA.Close()
	peerB.Close()

	published := newest - firstSeq
	t.Logf("capacity: %s, %d segments of %d KiB published per channel", d, published, segSize>>10)
	if want := int(d/segDur) / 2; published < want {
		t.Errorf("the fixture published %d segments, want at least %d: the test goroutine was starved", published, want)
	}
	for _, v := range viewers {
		t.Logf("capacity: viewer %s: segments %d-%d (%d), max latency %s, %d errors", v.ch, v.first, v.last, v.segments, v.maxLatency, v.failures)
		for _, err := range v.errors() {
			t.Error(err)
		}
		if v.failures > len(v.errors()) {
			t.Errorf("%s: %d more errors", v.ch, v.failures-len(v.errors()))
		}
		// A viewer within stallBound is at most that many segments
		// behind when it stops.
		if behind := int(stallBound / segDur); v.last < newest-behind {
			t.Errorf("%s: last segment %d, want at least %d: the viewer fell behind", v.ch, v.last, newest-behind)
		}
	}
	for _, s := range samples {
		t.Logf("capacity: at %s: %d goroutines, heap in use %.1f MiB", s.at.Round(time.Second), s.goroutines, mib(s.heap))
	}
	for _, p := range checkGrowth(samples, warmup) {
		t.Error(p)
	}
	goleak.VerifyNone(t, baseline)
}

// writeConfig writes a config dir from relative path -> content and
// returns its path.
func writeConfig(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}
