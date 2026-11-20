// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/KingOfKalk/wg-stream-proxy/internal/config"
	"github.com/KingOfKalk/wg-stream-proxy/internal/probe"
)

// probeCallTimeout bounds the request to POST /debug/probe: the server
// stops a probe after 60s.
const probeCallTimeout = 75 * time.Second

// headerList is the repeatable --header flag: Name=Value pairs.
type headerList struct{ h http.Header }

func (l *headerList) String() string { return "" }

// Set adds one Name=Value pair (probe.SetHeader has the rules).
func (l *headerList) Set(s string) error {
	name, value, ok := strings.Cut(s, "=")
	if !ok {
		return errors.New("want Name=Value")
	}
	if l.h == nil {
		l.h = http.Header{}
	}
	return probe.SetHeader(l.h, name, value)
}

// probeFlags are the flags of probe.
type probeFlags struct {
	fset    *flag.FlagSet
	config  *config.Flags
	route   string
	headers headerList
	json    bool
}

func newProbeFlags() *probeFlags {
	fs, cf := configFlagSet("probe")
	f := &probeFlags{fset: fs, config: cf}
	fs.StringVar(&f.route, "route", config.RouteDirect, "route to test: direct or a tunnel name")
	fs.Var(&f.headers, "header", "upstream request header Name=Value, may be repeated (like upstream.headers of a channel)")
	fs.BoolVar(&f.json, "json", false, "print the result as JSON")
	return f
}

// parse parses args; the URL may come before, between or after the
// flags. It returns the URL and the exit code and ok=false if the
// command must stop.
func (f *probeFlags) parse(c *cli, args []string) (target string, code int, ok bool) {
	f.fset.SetOutput(c.stderr)
	var rest []string
	for {
		if err := f.fset.Parse(args); err != nil {
			return "", c.parseFailed("probe", err), false
		}
		if f.fset.NArg() == 0 {
			break
		}
		rest = append(rest, f.fset.Arg(0))
		args = f.fset.Args()[1:]
	}
	if len(rest) != 1 {
		_, _ = fmt.Fprintln(c.stderr, "wg-stream-proxy: probe: want exactly one URL")
		return "", 2, false
	}
	if err := probe.CheckURL(rest[0]); err != nil {
		// The argument is not echoed: it may carry a token.
		_, _ = fmt.Fprintln(c.stderr, "wg-stream-proxy: probe: "+err.Error())
		return "", 2, false
	}
	return rest[0], 0, true
}

// probeCmd tests a URL through a route (spec 7.2): the running proxy
// fetches it like it would serve it (POST /debug/probe) and the command
// reports the detected format and the timing. It needs [debug] listen and
// a running proxy. Exit code 0 if the proxy would serve the URL, 1 if not,
// 2 for usage errors.
func (c *cli) probeCmd(args []string) int {
	f := newProbeFlags()
	target, code, ok := f.parse(c, args)
	if !ok {
		return code
	}
	opts, err := config.Resolve(*f.config, c.env)
	if err != nil {
		c.printErrors(err)
		return 1
	}
	// Only the debug listener address is needed, read without the tunnel
	// and channel files (spec 7.2): the running proxy answers with its
	// own loaded config, so a broken file of the newer version does not
	// stop the command.
	_, debugListen, err := config.LoadAddresses(opts)
	if err != nil {
		c.printErrors(err)
		return 1
	}
	if debugListen == "" {
		return c.probeFail(1, "[debug] listen is not set: set it in config.toml and start the proxy (probe uses the running proxy)")
	}
	return c.probeViaProxy(debugListen, f, target)
}

// probeViaProxy sends the probe to the debug listener of the running
// proxy. An answer with an error is reported.
func (c *cli) probeViaProxy(debugListen string, f *probeFlags, target string) int {
	base, err := localURL(debugListen, "")
	if err != nil {
		return c.probeFail(1, err.Error())
	}
	req := struct {
		Route   string            `json:"route"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers,omitempty"`
	}{Route: f.route, URL: target}
	if len(f.headers.h) > 0 {
		req.Headers = make(map[string]string, len(f.headers.h))
		for k, v := range f.headers.h {
			req.Headers[k] = v[0]
		}
	}
	var rep probe.Report
	err = c.debugCall(http.MethodPost, base+"/debug/probe", probeCallTimeout, req, &rep)
	var se *statusError
	var de *dialError
	switch {
	case errors.As(err, &de):
		return c.probeFail(1, "nothing listens on [debug] listen "+debugListen+": start the proxy")
	case errors.As(err, &se):
		return c.probeStatusFailed(se)
	case err != nil:
		// The URL is in the request body: it is not in this error.
		return c.probeFail(1, "the running proxy: "+err.Error())
	}
	return c.probeReport(&rep, f)
}

// probeStatusFailed reports an error answer of the debug listener.
func (c *cli) probeStatusFailed(se *statusError) int {
	switch {
	case se.code == http.StatusNotFound && se.msg == "":
		return c.probeFail(1, "the running proxy does not know POST /debug/probe (older version?)")
	case se.msg == "":
		return c.probeFail(1, fmt.Sprintf("the running proxy answered status %d", se.code))
	case se.code == http.StatusBadRequest || se.code == http.StatusNotFound:
		return c.probeFail(2, "the running proxy: "+se.msg)
	}
	return c.probeFail(1, "the running proxy: "+se.msg)
}

func (c *cli) probeFail(code int, msg string) int {
	_, _ = fmt.Fprintln(c.stderr, "wg-stream-proxy: probe: "+msg)
	return code
}

// probeReport prints rep and returns the exit code.
func (c *cli) probeReport(rep *probe.Report, f *probeFlags) int {
	write := rep.WriteText
	if f.json {
		write = rep.WriteJSON
	}
	if err := write(c.stdout); err != nil {
		return c.probeFail(1, err.Error())
	}
	if !rep.OK {
		return 1
	}
	return 0
}
