package main

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"strconv"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/control"
	"kaiak/internal/server"
	"kaiak/internal/telemetry/otlp"
)

// settings is the process configuration read from the environment
// (docs/specs/GATEWAY.md, Configuration sources).
type settings struct {
	// configFile is set in file mode; control in control-plane mode. Exactly one is.
	configFile string
	// control is control-plane mode's client options as the environment sets them:
	// where the control plane is (URL), the token the gateway presents, how long boot
	// waits for the stream's first config (BootWait), and the seed config (SeedConfig,
	// SeedFile; nil when none is set).
	control    *control.Options
	instanceID string
	listenAddr string
	adminAddr  string
	drain      server.DrainTimes
	// drainReserve is the end of the drain timeout kept for the usage flush and the
	// final status (control-plane mode): in-flight requests are cut that much before
	// the timeout.
	drainReserve time.Duration
	// client bounds client progress on both listeners.
	client server.ClientTimeouts
	// bodyMemory is the body budget: the request body bytes held at once.
	bodyMemory int64
	// usageMemory bounds the encoded usage records held in memory waiting for
	// delivery (control-plane mode).
	usageMemory int64
	// maxConnections caps the API listener's open connections; 0 is no cap.
	maxConnections int64
	// metricsToken is the bearer token /metrics requires; "" leaves it open.
	metricsToken string
	// logExport is where log records also go over OTLP, metricExport where metrics
	// are pushed; each nil when that signal's export is off.
	logExport    *otlp.Settings
	metricExport *otlp.Settings
}

// Default listen addresses: the API port and the admin port.
const (
	defaultListenAddr = ":8080"
	defaultAdminAddr  = ":9090"
)

// Default drain waits: the grace period covers load balancers taking the instance out
// of rotation; the timeout lets long streams finish. The flush reserve is the end of
// the timeout kept for the usage flush and the final status; left unset, it is at
// most half the timeout.
const (
	defaultDrainGrace   = 5 * time.Second
	defaultDrainTimeout = 60 * time.Second
	defaultDrainReserve = 10 * time.Second
)

func readSettings(lookupEnv func(string) (string, bool)) (settings, error) {
	s := settings{listenAddr: defaultListenAddr, adminAddr: defaultAdminAddr,
		drain: server.DrainTimes{Grace: defaultDrainGrace, Timeout: defaultDrainTimeout}, client: server.DefaultClientTimeouts,
		bodyMemory: server.DefaultBodyMemory, usageMemory: control.DefaultUsageMemoryBytes}
	var err error
	s.configFile, _ = lookupEnv("KAIAK_CONFIG_FILE")
	seedFile, _ := lookupEnv("KAIAK_SEED_CONFIG_FILE")
	if s.control, err = readControlSettings(lookupEnv, s.configFile != "", seedFile); err != nil {
		return s, err
	}
	if s.configFile == "" && s.control == nil {
		return s, errors.New("no config source: set KAIAK_CONFIG_FILE (file mode), or KAIAK_CONTROL_URL and KAIAK_CONTROL_TOKEN (control-plane mode)")
	}
	if seedFile != "" && s.control == nil {
		return s, errors.New("KAIAK_SEED_CONFIG_FILE is for control-plane mode: in file mode KAIAK_CONFIG_FILE is the config")
	}
	if addr, _ := lookupEnv("KAIAK_LISTEN_ADDR"); addr != "" {
		s.listenAddr = addr
	}
	if addr, _ := lookupEnv("KAIAK_ADMIN_ADDR"); addr != "" {
		s.adminAddr = addr
	}
	if s.drain.Grace, err = durationMS(lookupEnv, "KAIAK_DRAIN_GRACE_MS", s.drain.Grace, 0); err != nil {
		return s, err
	}
	if s.drain.Timeout, err = durationMS(lookupEnv, "KAIAK_DRAIN_TIMEOUT_MS", s.drain.Timeout, 0); err != nil {
		return s, err
	}
	if s.drainReserve, err = durationMS(lookupEnv, "KAIAK_DRAIN_FLUSH_RESERVE_MS", min(defaultDrainReserve, s.drain.Timeout/2), 0); err != nil {
		return s, err
	}
	if s.drainReserve > s.drain.Timeout {
		return s, fmt.Errorf("KAIAK_DRAIN_FLUSH_RESERVE_MS=%d is above the drain timeout (%d ms): the reserve is the end of the timeout",
			s.drainReserve.Milliseconds(), s.drain.Timeout.Milliseconds())
	}
	// A client bound of 0 would be no bound at all.
	for _, t := range []struct {
		name string
		d    *time.Duration
	}{
		{"KAIAK_IDLE_TIMEOUT_MS", &s.client.Idle},
		{"KAIAK_BODY_READ_TIMEOUT_MS", &s.client.BodyRead},
		{"KAIAK_WRITE_TIMEOUT_MS", &s.client.Write},
	} {
		if *t.d, err = durationMS(lookupEnv, t.name, *t.d, 1); err != nil {
			return s, err
		}
	}
	for _, n := range []struct {
		name  string
		v     *int64
		least int64
	}{
		{"KAIAK_BODY_MEMORY_BYTES", &s.bodyMemory, 1},
		{"KAIAK_USAGE_MEMORY_BYTES", &s.usageMemory, 1},
		{"KAIAK_MAX_CONNECTIONS", &s.maxConnections, 0},
	} {
		if *n.v, err = wholeNumber(lookupEnv, n.name, *n.v, n.least); err != nil {
			return s, err
		}
	}
	s.metricsToken, _ = lookupEnv("KAIAK_METRICS_TOKEN")
	if s.logExport, err = otlp.ReadSettings(otlp.Logs, lookupEnv); err != nil {
		return s, err
	}
	if s.metricExport, err = otlp.ReadSettings(otlp.Metrics, lookupEnv); err != nil {
		return s, err
	}
	s.instanceID, _ = lookupEnv("KAIAK_INSTANCE_ID")
	if s.instanceID == "" {
		host, err := os.Hostname()
		if err != nil {
			return s, fmt.Errorf("KAIAK_INSTANCE_ID is not set and the hostname is unknown: %w", err)
		}
		s.instanceID = host
	}
	// The instance ID travels in the Kaiak-Instance header and in every usage record;
	// a control plane refuses one of another shape.
	if s.control != nil && !control.IsInstanceID(s.instanceID) {
		return s, fmt.Errorf("instance ID %q (KAIAK_INSTANCE_ID, default the hostname) is not valid in control-plane mode: "+
			"want a letter or digit, then up to 252 letters, digits, '.', '_' or '-'", s.instanceID)
	}
	return s, nil
}

// configSource is where the config comes from, as the `kaiak starting` line says it.
func (s settings) configSource() []any {
	if s.control != nil {
		return []any{"kaiak.control.url", s.control.URL.Redacted()}
	}
	return []any{"kaiak.config.file", s.configFile}
}

// readControlSettings reads control-plane mode's variables into the client options
// they set: nil when none is set. The URL and token come together, and never beside a
// config file (fileMode). seedFile is KAIAK_SEED_CONFIG_FILE. The boot wait bounds the
// boot's wait for the stream's first config — the stream opened again while the
// control plane is unavailable — before the seed config is used; what is left of it
// bounds the wait for the first totals.
func readControlSettings(lookupEnv func(string) (string, bool), fileMode bool, seedFile string) (*control.Options, error) {
	rawURL, _ := lookupEnv("KAIAK_CONTROL_URL")
	token, _ := lookupEnv("KAIAK_CONTROL_TOKEN")
	switch {
	case rawURL == "" && token == "":
		return nil, nil
	case fileMode && rawURL != "":
		return nil, errors.New("KAIAK_CONFIG_FILE and KAIAK_CONTROL_URL are both set: choose file mode or control-plane mode")
	case rawURL == "":
		return nil, errors.New("KAIAK_CONTROL_TOKEN is set without KAIAK_CONTROL_URL")
	case token == "":
		return nil, errors.New("KAIAK_CONTROL_URL is set without KAIAK_CONTROL_TOKEN")
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("KAIAK_CONTROL_URL: want an http:// or https:// base URL with a host and no query")
	}
	bootWait, err := durationMS(lookupEnv, "KAIAK_CONTROL_BOOT_WAIT_MS", control.DefaultBootWait, 1)
	if err != nil {
		return nil, err
	}
	opts := &control.Options{URL: u, Token: token, BootWait: bootWait}
	if seedFile != "" {
		if opts.SeedConfig, err = readSeed(seedFile, lookupEnv); err != nil {
			return nil, err
		}
		opts.SeedFile = seedFile
	}
	return opts, nil
}

// readSeed reads and checks the seed config: a seed that could not be applied must
// fail the start, not the outage it is kept for — so it is checked completely,
// backend credentials included. It may hold no priced model: it serves while the
// control plane, which keeps the budgets, is unavailable.
func readSeed(path string, lookupEnv func(string) (string, bool)) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("KAIAK_SEED_CONFIG_FILE: %w", err)
	}
	snapshot, err := config.Check(data, lookupEnv)
	if err != nil {
		return nil, fmt.Errorf("KAIAK_SEED_CONFIG_FILE %s: %w", path, err)
	}
	for _, name := range snapshot.ModelNames {
		if len(snapshot.Models[name].Prices) > 0 {
			return nil, fmt.Errorf("KAIAK_SEED_CONFIG_FILE %s: model %q is priced: the seed serves only free models, "+
				"since budgets need the control plane", path, name)
		}
	}
	return data, nil
}

// wholeNumber reads a whole number, least or more, from the environment variable
// name; unset or empty keeps def.
func wholeNumber(lookupEnv func(string) (string, bool), name string, def, least int64) (int64, error) {
	v, _ := lookupEnv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < least {
		return 0, fmt.Errorf("%s=%q: want a whole number, %d or more", name, v, least)
	}
	return n, nil
}

// durationMS reads a whole number of milliseconds, least (0 or more) or more, from
// the environment variable name; unset or empty keeps def.
func durationMS(lookupEnv func(string) (string, bool), name string, def time.Duration, least int64) (time.Duration, error) {
	v, _ := lookupEnv(name)
	if v == "" {
		return def, nil
	}
	ms, err := strconv.ParseInt(v, 10, 64)
	if err != nil || ms < 0 || ms > math.MaxInt64/int64(time.Millisecond) {
		return 0, fmt.Errorf("%s=%q: want a whole number of milliseconds, 0 or more", name, v)
	}
	if ms < least {
		return 0, fmt.Errorf("%s=%d: want a whole number of milliseconds above %d", name, ms, least-1)
	}
	return time.Duration(ms) * time.Millisecond, nil
}
