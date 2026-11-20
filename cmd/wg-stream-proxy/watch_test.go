// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/route"
)

// manualClock is a route.Clock whose timers fire only in tick; each
// AfterFunc call is reported on armed.
type manualClock struct {
	mu     sync.Mutex
	timers []*manualTimer
	calls  int
	armed  chan struct{}
}

type manualTimer struct {
	c      *manualClock
	f      func()
	active bool
}

func newManualClock() *manualClock { return &manualClock{armed: make(chan struct{}, 1000)} }

func (c *manualClock) AfterFunc(_ time.Duration, f func()) route.Timer {
	c.mu.Lock()
	t := &manualTimer{c: c, f: f, active: true}
	c.timers = append(c.timers, t)
	c.calls++
	c.mu.Unlock()
	c.armed <- struct{}{}
	return t
}

func (t *manualTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := t.active
	t.active = false
	return was
}

func (t *manualTimer) Reset(time.Duration) bool { panic("not used") }

func (c *manualClock) waitArmed(t *testing.T) {
	t.Helper()
	select {
	case <-c.armed:
	case <-time.After(10 * time.Second):
		t.Error("the watcher did not arm its timer")
	}
}

// tick fires the armed timer and waits until the watcher has finished
// its step.
func (c *manualClock) tick(t *testing.T) {
	t.Helper()
	c.mu.Lock()
	var f func()
	for _, tm := range c.timers {
		if tm.active {
			tm.active = false
			f = tm.f
		}
	}
	c.mu.Unlock()
	if f == nil {
		t.Error("no timer armed")
		return
	}
	f()
	c.waitArmed(t)
}

func (c *manualClock) timerCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// watchServe starts serve with the files of dir and a manual clock and
// runs test in a goroutine once the server listens. The test gets the
// playlist URL and the results of the reloads; serve ends when it
// returns.
func watchServe(t *testing.T, dir string, test func(list string, clk *manualClock, reloads <-chan error)) (*manualClock, *syncBuffer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, _, errOut := testCLI(nil)
	c.ctx = ctx
	c.hangup = make(chan os.Signal)
	clk := newManualClock()
	c.clock = clk
	reloads := make(chan error, 100)
	c.reloaded = func(err error) { reloads <- err }
	c.listening = func(addr net.Addr) {
		go func() {
			defer cancel()
			test("http://"+addr.String()+"/playlist.m3u8", clk, reloads)
		}()
	}
	if code := c.run([]string{"--config-dir", dir, "--listen", "127.0.0.1:0"}); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, errOut)
	}
	return clk, errOut
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Error(err)
	}
}

// TestServeReloadsOnFileChange: a changed channel file is picked up
// without SIGHUP once it is stable; a broken file keeps the old config
// serving and is reported once, not at every check.
func TestServeReloadsOnFileChange(t *testing.T) {
	channel := func(name string) string {
		return "[[channel]]\nname = \"" + name + "\"\nurl = \"https://tv.example.com/" + name + ".m3u8\"\n"
	}
	dir := writeDir(t, map[string]string{"channels.d/tv.toml": "tunnel = \"direct\"\n" + channel("News")})
	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()
	var before, added, broken string
	var results []error
	var extra int
	_, errOut := watchServe(t, dir, func(list string, clk *manualClock, reloads <-chan error) {
		clk.waitArmed(t)
		before = get(t, client, list)

		// A change is applied after it was stable for one more check.
		writeFile(t, filepath.Join(dir, "channels.d", "tv.toml"), "tunnel = \"direct\"\n"+channel("News")+channel("Sport"))
		clk.tick(t)
		select {
		case err := <-reloads:
			t.Errorf("reloaded at the first sight of the change: %v", err)
		default:
		}
		clk.tick(t)
		results = append(results, <-reloads)
		added = get(t, client, list)

		// A broken file: rejected once, the old config keeps serving.
		writeFile(t, filepath.Join(dir, "config.toml"), "bogus = 1\n")
		for range 6 {
			clk.tick(t)
		}
		results = append(results, <-reloads)
		broken = get(t, client, list)
		extra = len(reloads) // reloads beyond the two above
	})
	if strings.Contains(before, "/c/sport") || !strings.Contains(added, "/c/sport") {
		t.Errorf("channel list before the change:\n%s\nafter:\n%s", before, added)
	}
	if !strings.Contains(broken, "/c/sport") {
		t.Errorf("channel list after the broken file lacks the old channels:\n%s", broken)
	}
	if len(results) != 2 || results[0] != nil || results[1] == nil || extra != 0 {
		t.Errorf("reload results = %v and %d more, want [nil, error] and none", results, extra)
	}
	logs := errOut.String()
	if n := strings.Count(logs, "unknown key"); n != 1 {
		t.Errorf("the broken file was reported %d times, want once:\n%s", n, logs)
	}
	if n := strings.Count(logs, "msg=reloaded"); n != 1 {
		t.Errorf("successful reloads = %d, want 1:\n%s", n, logs)
	}
	if n := strings.Count(logs, "config files changed: reloading"); n != 2 {
		t.Errorf("change lines = %d, want 2:\n%s", n, logs)
	}
}

// TestServeWatchOff: [reload] watch_interval = "0" starts no watcher.
func TestServeWatchOff(t *testing.T) {
	dir := writeDir(t, map[string]string{
		"config.toml":        "[reload]\nwatch_interval = \"0\"\n",
		"channels.d/tv.toml": "tunnel = \"direct\"\n[[channel]]\nname = \"News\"\nurl = \"https://tv.example.com/news.m3u8\"\n",
	})
	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()
	clk, _ := watchServe(t, dir, func(list string, _ *manualClock, _ <-chan error) {
		writeFile(t, filepath.Join(dir, "channels.d", "tv.toml"), "tunnel = \"direct\"\n")
		_ = get(t, client, list)
	})
	// Run has returned, so a watcher would have armed its timer.
	if n := clk.timerCalls(); n != 0 {
		t.Errorf("timers armed = %d, want 0", n)
	}
}
