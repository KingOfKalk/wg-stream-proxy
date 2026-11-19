// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
)

const (
	// tunnelListTimeout bounds the request of tunnel list.
	tunnelListTimeout = 3 * time.Second
	// tunnelStopTimeout bounds the request of tunnel stop: the cool-down
	// answers once the tunnel is stopped, after a drain of up to 30s.
	tunnelStopTimeout = 45 * time.Second
	// maxDebugBody bounds the debug responses read.
	maxDebugBody = 1 << 20
)

const tunnelUsage = "usage: wg-stream-proxy tunnel list | tunnel stop <name> [--config-dir DIR]"

// statusError: the debug listener answered with another status than 200.
type statusError struct {
	method, target string
	code           int
	// msg is the "error" of a JSON error body, if there is one.
	msg string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("%s %s: status %d", e.method, e.target, e.code)
}

// dialError: the connection to the debug listener could not be made, for
// example because nothing listens there.
type dialError struct{ err error }

func (e *dialError) Error() string { return e.err.Error() }
func (e *dialError) Unwrap() error { return e.err }

// tunnelCmd lists the tunnels or cools one down through the debug
// endpoints of the running proxy (spec 7.2). It reads [debug] listen
// like healthcheck reads listen, so it works through docker exec in an
// image without shell or curl.
func (c *cli) tunnelCmd(args []string) int {
	if len(args) > 0 && isHelpFlag(args[0]) {
		return c.help("tunnel")
	}
	if len(args) == 0 {
		return c.tunnelUsage()
	}
	switch sub, args := args[0], args[1:]; sub {
	case "list":
		opts, code, ok := c.parseFlags("tunnel list", args)
		if !ok {
			return code
		}
		return c.tunnelList(opts)
	case "stop":
		// The name may come before or after the flags.
		var name string
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			name, args = args[0], args[1:]
		}
		maxArgs := 1
		if name != "" {
			maxArgs = 0
		}
		_, opts, rest, code, ok := c.parseFlagArgs("tunnel stop", args, maxArgs)
		if !ok {
			return code
		}
		if len(rest) == 1 {
			name = rest[0]
		}
		if name == "" {
			return c.tunnelUsage()
		}
		if !config.ValidName(name) {
			return c.tunnelFail(2, fmt.Sprintf("invalid tunnel name %q", name))
		}
		return c.tunnelStop(opts, name)
	default:
		return c.tunnelUsage()
	}
}

func (c *cli) tunnelUsage() int {
	_, _ = fmt.Fprintln(c.stderr, tunnelUsage)
	return 2
}

func (c *cli) tunnelFail(code int, msg string) int {
	_, _ = fmt.Fprintln(c.stderr, "wg-stream-proxy: tunnel: "+msg)
	return code
}

// debugBase returns the base URL of the debug listener of the config.
func (c *cli) debugBase(opts config.Options) (string, error) {
	cfg, err := config.Load(opts)
	if err != nil {
		return "", errors.New(firstLine(err))
	}
	if cfg.DebugListen() == "" {
		return "", errors.New("the debug listener is off: set [debug] listen in config.toml")
	}
	return localURL(cfg.DebugListen(), "")
}

// tunnelList prints the tunnels of GET /debug/tunnels as a table.
func (c *cli) tunnelList(opts config.Options) int {
	base, err := c.debugBase(opts)
	if err != nil {
		return c.tunnelFail(1, err.Error())
	}
	var resp struct {
		Tunnels []struct {
			Name           string    `json:"name"`
			State          string    `json:"state"`
			StartedAt      time.Time `json:"started_at"`
			LastHandshake  time.Time `json:"last_handshake"`
			ActiveRequests int       `json:"active_requests"`
		} `json:"tunnels"`
	}
	if err := c.debugCall(http.MethodGet, base+"/debug/tunnels", tunnelListTimeout, nil, &resp); err != nil {
		return c.tunnelFail(1, err.Error())
	}
	when := func(t time.Time) string {
		if t.IsZero() {
			return "-"
		}
		return t.Format(time.RFC3339)
	}
	w := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAME\tSTATE\tREQUESTS\tSTARTED\tLAST HANDSHAKE")
	for _, t := range resp.Tunnels {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n", t.Name, t.State, t.ActiveRequests, when(t.StartedAt), when(t.LastHandshake))
	}
	if err := w.Flush(); err != nil {
		return c.tunnelFail(1, err.Error())
	}
	return 0
}

// tunnelStop cools the tunnel name down with POST
// /debug/tunnels/{name}/stop and waits until it is stopped.
func (c *cli) tunnelStop(opts config.Options, name string) int {
	base, err := c.debugBase(opts)
	if err != nil {
		return c.tunnelFail(1, err.Error())
	}
	var resp struct {
		WasRunning bool `json:"was_running"`
	}
	err = c.debugCall(http.MethodPost, base+"/debug/tunnels/"+name+"/stop", tunnelStopTimeout, nil, &resp)
	var se *statusError
	switch {
	case errors.As(err, &se) && se.code == http.StatusNotFound:
		return c.tunnelFail(1, fmt.Sprintf("no tunnel named %q", name))
	case err != nil:
		return c.tunnelFail(1, err.Error())
	case resp.WasRunning:
		_, _ = fmt.Fprintf(c.stdout, "tunnel %s stopped\n", name)
	default:
		_, _ = fmt.Fprintf(c.stdout, "tunnel %s was not running\n", name)
	}
	return 0
}

// debugCall sends a request to the debug listener, with body (if not nil)
// as JSON, and decodes a 200 JSON answer into v; another status gives a
// *statusError, a failed connection a *dialError. Redirects are not
// followed.
func (c *cli) debugCall(method, target string, timeout time.Duration, body, v any) error {
	if c.tunnelTimeout > 0 {
		timeout = c.tunnelTimeout
	}
	ctx, cancel := context.WithTimeout(c.ctx, timeout)
	defer cancel()
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reqBody)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{
		Transport: &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		var oe *net.OpError
		if errors.As(err, &oe) && oe.Op == "dial" {
			return &dialError{err}
		}
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	answer := io.LimitReader(resp.Body, maxDebugBody)
	if resp.StatusCode != http.StatusOK {
		se := &statusError{method: method, target: target, code: resp.StatusCode}
		var e struct {
			Error string `json:"error"`
		}
		if json.NewDecoder(answer).Decode(&e) == nil {
			se.msg = e.Error
		}
		_, _ = io.Copy(io.Discard, answer)
		return se
	}
	if err := json.NewDecoder(answer).Decode(v); err != nil {
		return fmt.Errorf("%s %s: invalid answer: %w", method, target, err)
	}
	return nil
}
