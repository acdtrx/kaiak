package otlp

import (
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Settings are one signal's export settings read from the standard OTEL_*
// environment variables (docs/specs/GATEWAY.md, Configuration sources: OTLP export),
// with the resource every signal shares. The headers can carry credentials: no
// method prints them.
type Settings struct {
	signal   Signal
	endpoint *url.URL
	headers  []header
	timeout  time.Duration
	// serviceName is the resource's service.name.
	serviceName string
	// resource is OTEL_RESOURCE_ATTRIBUTES less service.name, service.version and
	// service.instance.id, which the gateway sets itself.
	resource []resourceAttr
}

type header struct{ name, value string }

type resourceAttr struct{ key, value string }

// The specification's defaults: a collector beside the gateway, 10 s per export.
const (
	defaultHost        = "http://localhost:4318"
	defaultTimeout     = 10 * time.Second
	defaultServiceName = "kaiak"
)

// ReadSettings reads sig's export settings from the environment through lookupEnv.
// It returns nil when the signal's export is off: no endpoint and no
// OTEL_<SIGNAL>_EXPORTER=otlp, or OTEL_<SIGNAL>_EXPORTER=none, or
// OTEL_SDK_DISABLED=true. For each setting the signal's variable wins over the
// general one. An empty variable counts as unset, as the OpenTelemetry
// specification has it. A malformed value is an error naming the variable; an error
// about headers never holds their values. The resource (OTEL_SERVICE_NAME,
// OTEL_RESOURCE_ATTRIBUTES) is read the same way for every signal, and only when
// the signal's export is on.
func ReadSettings(sig Signal, lookupEnv func(string) (string, bool)) (*Settings, error) {
	names := sig.info().variables
	get := func(name string) string {
		v, _ := lookupEnv(name)
		return v
	}
	var disabled bool
	switch v := get("OTEL_SDK_DISABLED"); strings.ToLower(v) {
	case "", "false":
	case "true":
		disabled = true
	default:
		return nil, fmt.Errorf("OTEL_SDK_DISABLED=%q: want true or false", v)
	}
	exporter := get(names.exporter)
	switch strings.ToLower(exporter) {
	case "", "otlp", "none":
	default:
		return nil, fmt.Errorf("%s=%q: want otlp or none", names.exporter, exporter)
	}
	if disabled || strings.EqualFold(exporter, "none") {
		return nil, nil
	}

	endpoint, err := readEndpoint(sig, get)
	if err != nil {
		return nil, err
	}
	if endpoint == nil {
		if !strings.EqualFold(exporter, "otlp") {
			return nil, nil
		}
		endpoint, _ = url.Parse(defaultHost)
		endpoint = endpoint.JoinPath(sig.info().path)
	}
	s := &Settings{signal: sig, endpoint: endpoint, timeout: defaultTimeout}

	if name, v := first(get, names.protocol, "OTEL_EXPORTER_OTLP_PROTOCOL"); v != "" {
		switch strings.ToLower(v) {
		case "http/json":
		case "grpc", "http/protobuf":
			return nil, fmt.Errorf("%s=%q: only http/json is supported (the gateway writes OTLP/HTTP with JSON encoding)", name, v)
		default:
			return nil, fmt.Errorf("%s=%q: want http/json", name, v)
		}
	}
	if name, v := first(get, names.headers, "OTEL_EXPORTER_OTLP_HEADERS"); v != "" {
		if s.headers, err = parseHeaders(name, v); err != nil {
			return nil, err
		}
	}
	if name, v := first(get, names.timeout, "OTEL_EXPORTER_OTLP_TIMEOUT"); v != "" {
		ms, err := strconv.ParseInt(v, 10, 64)
		if err != nil || ms <= 0 || ms > math.MaxInt64/int64(time.Millisecond) {
			return nil, fmt.Errorf("%s=%q: want a whole number of milliseconds above 0", name, v)
		}
		s.timeout = time.Duration(ms) * time.Millisecond
	}

	if s.serviceName, s.resource, err = readResource(get); err != nil {
		return nil, err
	}
	return s, nil
}

// readResource reads the resource's settings, the same for every signal: the
// service name (OTEL_SERVICE_NAME, else a service.name in OTEL_RESOURCE_ATTRIBUTES,
// else kaiak) and the other attributes of OTEL_RESOURCE_ATTRIBUTES but the two the
// gateway sets itself.
func readResource(get func(string) string) (serviceName string, attrs []resourceAttr, err error) {
	var fromAttributes string
	if v := get("OTEL_RESOURCE_ATTRIBUTES"); v != "" {
		all, err := parseResourceAttributes(v)
		if err != nil {
			return "", nil, err
		}
		for _, a := range all {
			switch a.key {
			case "service.name":
				fromAttributes = a.value
			case "service.version", "service.instance.id":
			default:
				attrs = append(attrs, a)
			}
		}
	}
	serviceName = defaultServiceName
	if v := get("OTEL_SERVICE_NAME"); v != "" {
		serviceName = v
	} else if fromAttributes != "" {
		serviceName = fromAttributes
	}
	return serviceName, attrs, nil
}

// EndpointHost is the endpoint's host:port, the port filled in from the scheme when
// the URL has none — what the gateway may log about where the signal goes.
func (s *Settings) EndpointHost() string {
	port := s.endpoint.Port()
	if port == "" {
		port = "80"
		if s.endpoint.Scheme == "https" {
			port = "443"
		}
	}
	return net.JoinHostPort(s.endpoint.Hostname(), port)
}

// String describes the settings without the headers, so that printing them by
// mistake cannot leak a credential.
func (s *Settings) String() string {
	return fmt.Sprintf("otlp.Settings{signal: %s, endpoint: %s, headers: %d, timeout: %s}",
		s.signal, s.endpoint.Redacted(), len(s.headers), s.timeout)
}

// first returns the first of names whose value is set, with that value; the last
// name when none is.
func first(get func(string) string, names ...string) (name, value string) {
	for _, name = range names {
		if value = get(name); value != "" {
			return name, value
		}
	}
	return name, ""
}

// readEndpoint reads the signal's endpoint: the signal's variable as is, else the
// general one with the signal's path (v1/logs, v1/metrics) joined to its path; nil
// when neither is set.
func readEndpoint(sig Signal, get func(string) string) (*url.URL, error) {
	own := sig.info().variables.endpoint
	if v := get(own); v != "" {
		return parseEndpoint(own, v)
	}
	v := get("OTEL_EXPORTER_OTLP_ENDPOINT")
	if v == "" {
		return nil, nil
	}
	u, err := parseEndpoint("OTEL_EXPORTER_OTLP_ENDPOINT", v)
	if err != nil {
		return nil, err
	}
	return u.JoinPath(sig.info().path), nil
}

// parseEndpoint parses an absolute http:// or https:// URL. The error leaves the
// value out: a URL may carry credentials.
func parseEndpoint(name, v string) (*url.URL, error) {
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%s: want an absolute http:// or https:// URL", name)
	}
	return u, nil
}

// parseHeaders parses the specification's header list: key=value pairs separated by
// commas, spaces around each allowed, values percent-decoded. Empty members are
// skipped. Errors name the entry by its position, never its text.
func parseHeaders(name, v string) ([]header, error) {
	var headers []header
	for i, member := range strings.Split(v, ",") {
		member = strings.TrimSpace(member)
		if member == "" {
			continue
		}
		key, raw, ok := strings.Cut(member, "=")
		key = strings.TrimSpace(key)
		if !ok || !isToken(key) {
			return nil, fmt.Errorf("%s: entry %d: want key=value with a header name as the key", name, i+1)
		}
		value, err := url.PathUnescape(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("%s: entry %d: the value is not properly percent-encoded", name, i+1)
		}
		if !isHeaderValue(value) {
			return nil, fmt.Errorf("%s: entry %d: the value holds a control character", name, i+1)
		}
		headers = append(headers, header{http.CanonicalHeaderKey(key), value})
	}
	return headers, nil
}

// parseResourceAttributes parses OTEL_RESOURCE_ATTRIBUTES: key=value pairs separated
// by commas, keys and values percent-decoded (the specification has "," and "="
// percent-encoded in both). Empty members are skipped; a later key replaces an
// earlier one.
func parseResourceAttributes(v string) ([]resourceAttr, error) {
	const name = "OTEL_RESOURCE_ATTRIBUTES"
	var attrs []resourceAttr
	for i, member := range strings.Split(v, ",") {
		member = strings.TrimSpace(member)
		if member == "" {
			continue
		}
		rawKey, rawValue, ok := strings.Cut(member, "=")
		key, kerr := url.PathUnescape(strings.TrimSpace(rawKey))
		value, verr := url.PathUnescape(strings.TrimSpace(rawValue))
		if !ok || kerr != nil || verr != nil || key == "" {
			return nil, fmt.Errorf("%s: entry %d: want key=value, both percent-encoded, the key not empty", name, i+1)
		}
		attrs = replaceAttr(attrs, resourceAttr{key, value})
	}
	return attrs, nil
}

func replaceAttr(attrs []resourceAttr, a resourceAttr) []resourceAttr {
	for i := range attrs {
		if attrs[i].key == a.key {
			attrs[i].value = a.value
			return attrs
		}
	}
	return append(attrs, a)
}

// isToken reports whether s is an HTTP token (RFC 9110, 5.6.2): a valid header name.
func isToken(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range []byte(s) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0) {
			return false
		}
	}
	return true
}

// isHeaderValue reports whether s can be sent as a header value: no control
// character but tab.
func isHeaderValue(s string) bool {
	for _, c := range []byte(s) {
		if c < ' ' && c != '\t' || c == 0x7f {
			return false
		}
	}
	return true
}
