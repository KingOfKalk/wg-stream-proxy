// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
)

// defaultHealthTimeout bounds the whole request of the healthcheck.
const defaultHealthTimeout = 3 * time.Second

// healthcheck asks /healthz of the running proxy on the listen address
// (spec 7.2). It prints nothing on success and one line on failure. It
// needs no shell or curl, so it works as the HEALTHCHECK of a distroless
// image.
func (c *cli) healthcheck(args []string) int {
	opts, code, ok := c.parseFlags("healthcheck", args)
	if !ok {
		return code
	}
	cfg, err := config.Load(opts)
	if err != nil {
		return c.healthFail(firstLine(err))
	}
	target, err := healthcheckURL(cfg.Listen())
	if err != nil {
		return c.healthFail(err.Error())
	}
	if err := c.probe(target); err != nil {
		return c.healthFail(err.Error())
	}
	return 0
}

func (c *cli) healthFail(msg string) int {
	_, _ = fmt.Fprintln(c.stderr, "wg-stream-proxy: healthcheck: "+msg)
	return 1
}

// probe issues GET target and wants 200. Redirects are not followed.
func (c *cli) probe(target string) error {
	timeout := c.healthTimeout
	if timeout <= 0 {
		timeout = defaultHealthTimeout
	}
	ctx, cancel := context.WithTimeout(c.ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
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
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", target, resp.StatusCode)
	}
	return nil
}

// healthcheckURL returns the /healthz URL for a listen address.
func healthcheckURL(listen string) (string, error) {
	return localURL(listen, "/healthz")
}

// localURL returns the http URL of path on a listen address of this
// host. An unspecified host (":2349", "0.0.0.0", "::") means the loopback
// interface of the same family.
func localURL(listen, path string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("listen address %q: %w", listen, err)
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, port) + path, nil
}

// firstLine returns the first line of an error message; config errors
// are joined with line breaks.
func firstLine(err error) string {
	line, _, _ := strings.Cut(err.Error(), "\n")
	return line
}
