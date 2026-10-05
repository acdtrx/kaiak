package otlplog

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func envOf(vars map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

func mustSettings(t *testing.T, vars map[string]string) *Settings {
	t.Helper()
	s, err := ReadSettings(envOf(vars))
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	if s == nil {
		t.Fatal("ReadSettings: export off, want on")
	}
	return s
}

func TestExportOnOrOff(t *testing.T) {
	const general = "OTEL_EXPORTER_OTLP_ENDPOINT"
	const logs = "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"
	for _, tc := range []struct {
		name string
		vars map[string]string
		// endpoint is the URL posted to; "" means export is off.
		endpoint string
	}{
		{"nothing set", nil, ""},
		{"empty values count as unset", map[string]string{general: "", logs: "", "OTEL_LOGS_EXPORTER": ""}, ""},
		{"only settings other than the endpoint", map[string]string{"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc", "OTEL_SERVICE_NAME": "x"}, ""},
		{"logs endpoint, as is", map[string]string{logs: "http://collector:4318/custom"}, "http://collector:4318/custom"},
		{"logs endpoint with no path", map[string]string{logs: "https://collector.example.com"}, "https://collector.example.com"},
		{"logs endpoint keeps its query", map[string]string{logs: "http://c:4318/v1/logs?tenant=a"}, "http://c:4318/v1/logs?tenant=a"},
		{"general endpoint gets v1/logs", map[string]string{general: "http://collector:4318"}, "http://collector:4318/v1/logs"},
		{"general endpoint with a trailing slash", map[string]string{general: "http://collector:4318/"}, "http://collector:4318/v1/logs"},
		{"general endpoint with a base path", map[string]string{general: "http://collector:4318/mycollector/"}, "http://collector:4318/mycollector/v1/logs"},
		{"logs endpoint wins over general", map[string]string{general: "http://general:4318", logs: "http://logs:4318/v1/logs"}, "http://logs:4318/v1/logs"},
		{"otlp exporter with no endpoint: the default", map[string]string{"OTEL_LOGS_EXPORTER": "otlp"}, "http://localhost:4318/v1/logs"},
		{"otlp exporter in any case", map[string]string{"OTEL_LOGS_EXPORTER": "OTLP"}, "http://localhost:4318/v1/logs"},
		{"otlp exporter with an endpoint", map[string]string{"OTEL_LOGS_EXPORTER": "otlp", general: "http://c:4318"}, "http://c:4318/v1/logs"},
		{"none turns it off", map[string]string{"OTEL_LOGS_EXPORTER": "none", general: "http://c:4318", logs: "http://c:4318/v1/logs"}, ""},
		{"none turns it off before a malformed endpoint", map[string]string{"OTEL_LOGS_EXPORTER": "None", logs: "not a url"}, ""},
		{"SDK disabled turns it off", map[string]string{"OTEL_SDK_DISABLED": "true", logs: "http://c:4318/v1/logs"}, ""},
		{"SDK disabled in any case", map[string]string{"OTEL_SDK_DISABLED": "TRUE", "OTEL_LOGS_EXPORTER": "otlp"}, ""},
		{"SDK disabled false keeps it on", map[string]string{"OTEL_SDK_DISABLED": "False", general: "http://c:4318"}, "http://c:4318/v1/logs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := ReadSettings(envOf(tc.vars))
			if err != nil {
				t.Fatalf("ReadSettings: %v", err)
			}
			switch {
			case tc.endpoint == "" && s != nil:
				t.Fatalf("export on to %s, want off", s.endpoint)
			case tc.endpoint != "" && s == nil:
				t.Fatalf("export off, want on to %s", tc.endpoint)
			case s != nil && s.endpoint.String() != tc.endpoint:
				t.Fatalf("endpoint %s, want %s", s.endpoint, tc.endpoint)
			}
		})
	}
}

func TestSettingsFallbacksAndDefaults(t *testing.T) {
	const on = "http://c:4318"
	t.Run("defaults", func(t *testing.T) {
		s := mustSettings(t, map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": on})
		if s.timeout != 10*time.Second || len(s.headers) != 0 || s.serviceName != "kaiak" || len(s.resource) != 0 {
			t.Fatalf("got timeout %s, headers %d, service %q, resource %v; want 10s, none, kaiak, none",
				s.timeout, len(s.headers), s.serviceName, s.resource)
		}
	})
	t.Run("timeout: logs variable wins", func(t *testing.T) {
		s := mustSettings(t, map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": on,
			"OTEL_EXPORTER_OTLP_TIMEOUT": "3000", "OTEL_EXPORTER_OTLP_LOGS_TIMEOUT": "2500"})
		if s.timeout != 2500*time.Millisecond {
			t.Fatalf("timeout %s, want 2.5s", s.timeout)
		}
	})
	t.Run("timeout: general variable", func(t *testing.T) {
		s := mustSettings(t, map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": on, "OTEL_EXPORTER_OTLP_TIMEOUT": "3000"})
		if s.timeout != 3*time.Second {
			t.Fatalf("timeout %s, want 3s", s.timeout)
		}
	})
	t.Run("headers: logs variable wins, values decoded", func(t *testing.T) {
		s := mustSettings(t, map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": on,
			"OTEL_EXPORTER_OTLP_HEADERS":      "x-general=1",
			"OTEL_EXPORTER_OTLP_LOGS_HEADERS": " authorization = Bearer%20abc%3D ,, x-tenant=a=b ,",
		})
		want := []header{{"Authorization", "Bearer abc="}, {"X-Tenant", "a=b"}}
		if fmt.Sprint(s.headers) != fmt.Sprint(want) {
			t.Fatalf("headers %v, want %v", s.headers, want)
		}
	})
	t.Run("headers: general variable", func(t *testing.T) {
		s := mustSettings(t, map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": on, "OTEL_EXPORTER_OTLP_HEADERS": "api-key=k"})
		if want := []header{{"Api-Key", "k"}}; fmt.Sprint(s.headers) != fmt.Sprint(want) {
			t.Fatalf("headers %v, want %v", s.headers, want)
		}
	})
	t.Run("protocol: http/json, unset or in any case", func(t *testing.T) {
		for _, vars := range []map[string]string{
			{"OTEL_EXPORTER_OTLP_PROTOCOL": "http/json"},
			{"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL": "HTTP/JSON"},
			{"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc", "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL": "http/json"},
		} {
			vars["OTEL_EXPORTER_OTLP_ENDPOINT"] = on
			mustSettings(t, vars)
		}
	})
	t.Run("service name: OTEL_SERVICE_NAME wins", func(t *testing.T) {
		s := mustSettings(t, map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": on,
			"OTEL_SERVICE_NAME": "gw-eu", "OTEL_RESOURCE_ATTRIBUTES": "service.name=other"})
		if s.serviceName != "gw-eu" {
			t.Fatalf("service name %q, want gw-eu", s.serviceName)
		}
	})
	t.Run("service name: from the resource attributes", func(t *testing.T) {
		s := mustSettings(t, map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": on, "OTEL_RESOURCE_ATTRIBUTES": "service.name=other"})
		if s.serviceName != "other" || len(s.resource) != 0 {
			t.Fatalf("service name %q, resource %v; want other, none", s.serviceName, s.resource)
		}
	})
	t.Run("resource attributes: decoded, the gateway's own left out, later wins", func(t *testing.T) {
		s := mustSettings(t, map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": on,
			"OTEL_RESOURCE_ATTRIBUTES": "deployment.environment.name=staging, k8s%2Cpod=gw%2C1%3Dx ,service.version=9,service.instance.id=other,,deployment.environment.name=prod"})
		want := []resourceAttr{{"deployment.environment.name", "prod"}, {"k8s,pod", "gw,1=x"}}
		if fmt.Sprint(s.resource) != fmt.Sprint(want) {
			t.Fatalf("resource %v, want %v", s.resource, want)
		}
	})
}

func TestSettingsRefuseMalformedValues(t *testing.T) {
	const on = "http://c:4318"
	const secret = "s3cr3t-token"
	for _, tc := range []struct {
		name string
		vars map[string]string
		// want is in the error; the error never holds the secret.
		want string
	}{
		{"SDK disabled not a boolean", map[string]string{"OTEL_SDK_DISABLED": "yes"}, `OTEL_SDK_DISABLED="yes": want true or false`},
		{"unknown logs exporter", map[string]string{"OTEL_LOGS_EXPORTER": "console"}, `OTEL_LOGS_EXPORTER="console": want otlp or none`},
		{"logs exporter list", map[string]string{"OTEL_LOGS_EXPORTER": "otlp,console"}, `OTEL_LOGS_EXPORTER="otlp,console"`},
		{"logs endpoint not http", map[string]string{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "ftp://c/v1/logs"}, "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT: want an absolute http:// or https:// URL"},
		{"general endpoint relative", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "collector:4318"}, "OTEL_EXPORTER_OTLP_ENDPOINT: want an absolute"},
		{"endpoint with no host", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http:///v1"}, "OTEL_EXPORTER_OTLP_ENDPOINT: want an absolute"},
		{"endpoint unparsable, credentials not echoed", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://u:" + secret + "@c:43%18"}, "OTEL_EXPORTER_OTLP_ENDPOINT: want an absolute"},
		{"protocol grpc", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": on, "OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"}, `OTEL_EXPORTER_OTLP_PROTOCOL="grpc": only http/json is supported`},
		{"protocol http/protobuf", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": on, "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL": "http/protobuf"}, `OTEL_EXPORTER_OTLP_LOGS_PROTOCOL="http/protobuf": only http/json is supported`},
		{"protocol logs variable wins, grpc", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": on, "OTEL_EXPORTER_OTLP_PROTOCOL": "http/json", "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL": "grpc"}, `OTEL_EXPORTER_OTLP_LOGS_PROTOCOL="grpc": only http/json`},
		{"protocol unknown", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": on, "OTEL_EXPORTER_OTLP_PROTOCOL": "json"}, `OTEL_EXPORTER_OTLP_PROTOCOL="json": want http/json`},
		{"timeout zero", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": on, "OTEL_EXPORTER_OTLP_TIMEOUT": "0"}, `OTEL_EXPORTER_OTLP_TIMEOUT="0": want a whole number of milliseconds above 0`},
		{"timeout negative", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": on, "OTEL_EXPORTER_OTLP_LOGS_TIMEOUT": "-5"}, `OTEL_EXPORTER_OTLP_LOGS_TIMEOUT="-5"`},
		{"timeout not whole", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": on, "OTEL_EXPORTER_OTLP_TIMEOUT": "1.5"}, `OTEL_EXPORTER_OTLP_TIMEOUT="1.5"`},
		{"timeout with a unit", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": on, "OTEL_EXPORTER_OTLP_TIMEOUT": "10s"}, `OTEL_EXPORTER_OTLP_TIMEOUT="10s"`},
		{"timeout overflowing", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": on, "OTEL_EXPORTER_OTLP_TIMEOUT": "9223372036854775807"}, `OTEL_EXPORTER_OTLP_TIMEOUT=`},
		{"header without =", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": on, "OTEL_EXPORTER_OTLP_HEADERS": "a=1,Bearer " + secret}, "OTEL_EXPORTER_OTLP_HEADERS: entry 2: want key=value with a header name as the key"},
		{"header name not a token", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": on, "OTEL_EXPORTER_OTLP_LOGS_HEADERS": "bad key=" + secret}, "OTEL_EXPORTER_OTLP_LOGS_HEADERS: entry 1: want key=value"},
		{"header name empty", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": on, "OTEL_EXPORTER_OTLP_HEADERS": "=" + secret}, "OTEL_EXPORTER_OTLP_HEADERS: entry 1"},
		{"header value badly encoded", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": on, "OTEL_EXPORTER_OTLP_HEADERS": "authorization=" + secret + "%zz"}, "OTEL_EXPORTER_OTLP_HEADERS: entry 1: the value is not properly percent-encoded"},
		{"header value with a newline", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": on, "OTEL_EXPORTER_OTLP_HEADERS": "authorization=" + secret + "%0Ax"}, "OTEL_EXPORTER_OTLP_HEADERS: entry 1: the value holds a control character"},
		{"resource attribute without =", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": on, "OTEL_RESOURCE_ATTRIBUTES": "a=1,b"}, "OTEL_RESOURCE_ATTRIBUTES: entry 2: want key=value"},
		{"resource attribute empty key", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": on, "OTEL_RESOURCE_ATTRIBUTES": "=x"}, "OTEL_RESOURCE_ATTRIBUTES: entry 1"},
		{"resource attribute badly encoded", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": on, "OTEL_RESOURCE_ATTRIBUTES": "a=%g1"}, "OTEL_RESOURCE_ATTRIBUTES: entry 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := ReadSettings(envOf(tc.vars))
			if err == nil {
				t.Fatalf("ReadSettings = %v, want an error", s)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q, want it to hold %q", err, tc.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error %q echoes a secret value", err)
			}
		})
	}
}

func TestSettingsNeverPrintHeaders(t *testing.T) {
	const secret = "s3cr3t-token"
	s := mustSettings(t, map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://user:" + secret + "-pw@c:4318",
		"OTEL_EXPORTER_OTLP_HEADERS":  "authorization=Bearer%20" + secret,
	})
	var b strings.Builder
	slog.New(slog.NewTextHandler(&b, nil)).Info("settings", "s", s)
	slog.New(slog.NewJSONHandler(&b, nil)).Info("settings", "s", s)
	fmt.Fprintf(&b, "%v %+v %s", s, s, s)
	if strings.Contains(b.String(), secret) {
		t.Fatalf("printed settings hold a secret: %s", b.String())
	}
}

func TestEndpointHost(t *testing.T) {
	for endpoint, want := range map[string]string{
		"http://collector:4318/v1/logs":   "collector:4318",
		"http://collector/v1/logs":        "collector:80",
		"https://collector.example.com/x": "collector.example.com:443",
		"http://[::1]:4318/v1/logs":       "[::1]:4318",
	} {
		s := mustSettings(t, map[string]string{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": endpoint})
		if got := s.EndpointHost(); got != want {
			t.Errorf("EndpointHost(%s) = %s, want %s", endpoint, got, want)
		}
	}
}
