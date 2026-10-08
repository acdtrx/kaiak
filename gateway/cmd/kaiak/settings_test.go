package main

import (
	"strings"
	"testing"
	"time"

	"kaiak/internal/server"
)

func TestDrainTimeSettings(t *testing.T) {
	base := map[string]string{"KAIAK_CONFIG_FILE": "c.json", "KAIAK_INSTANCE_ID": "i"}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	s, err := readSettings(envOf(base))
	if err != nil {
		t.Fatal(err)
	}
	if s.drain.Grace != 5*time.Second || s.drain.Timeout != 60*time.Second {
		t.Errorf("defaults = %v, %v; want 5s, 1m0s", s.drain.Grace, s.drain.Timeout)
	}
	s, err = readSettings(envOf(with(map[string]string{"KAIAK_DRAIN_GRACE_MS": "0", "KAIAK_DRAIN_TIMEOUT_MS": "1500"})))
	if err != nil {
		t.Fatal(err)
	}
	if s.drain.Grace != 0 || s.drain.Timeout != 1500*time.Millisecond {
		t.Errorf("set = %v, %v; want 0s, 1.5s", s.drain.Grace, s.drain.Timeout)
	}
	for _, bad := range []string{"-1", "1.5", "5s", "abc", "99999999999999999999"} {
		for _, name := range []string{"KAIAK_DRAIN_GRACE_MS", "KAIAK_DRAIN_TIMEOUT_MS"} {
			if _, err := readSettings(envOf(with(map[string]string{name: bad}))); err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("%s=%q: got %v, want an error naming it", name, bad, err)
			}
		}
	}
}

func TestClientTimeoutSettings(t *testing.T) {
	base := map[string]string{"KAIAK_CONFIG_FILE": "c.json", "KAIAK_INSTANCE_ID": "i"}
	with := func(name, value string) map[string]string {
		m := map[string]string{name: value}
		for k, v := range base {
			m[k] = v
		}
		return m
	}
	s, err := readSettings(envOf(base))
	if err != nil {
		t.Fatal(err)
	}
	if s.client != server.DefaultClientTimeouts {
		t.Errorf("defaults = %+v, want %+v", s.client, server.DefaultClientTimeouts)
	}
	for name, got := range map[string]func(settings) time.Duration{
		"KAIAK_IDLE_TIMEOUT_MS":      func(s settings) time.Duration { return s.client.Idle },
		"KAIAK_BODY_READ_TIMEOUT_MS": func(s settings) time.Duration { return s.client.BodyRead },
		"KAIAK_WRITE_TIMEOUT_MS":     func(s settings) time.Duration { return s.client.Write },
	} {
		s, err := readSettings(envOf(with(name, "1500")))
		if err != nil || got(s) != 1500*time.Millisecond {
			t.Errorf("%s=1500: %v, %v; want 1.5s", name, got(s), err)
		}
		for _, bad := range []string{"0", "-1", "5s"} {
			if _, err := readSettings(envOf(with(name, bad))); err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("%s=%q: got %v, want an error naming it", name, bad, err)
			}
		}
	}
}

func TestListenAddressDefaults(t *testing.T) {
	s, err := readSettings(envOf(map[string]string{"KAIAK_CONFIG_FILE": "c.json", "KAIAK_INSTANCE_ID": "i"}))
	if err != nil {
		t.Fatal(err)
	}
	if s.listenAddr != ":8080" || s.adminAddr != ":9090" {
		t.Errorf("defaults = %q, %q; want :8080, :9090", s.listenAddr, s.adminAddr)
	}
}

func TestControlModeSettings(t *testing.T) {
	controlEnv := map[string]string{"KAIAK_CONTROL_URL": "https://cp.example:8443/base", "KAIAK_CONTROL_TOKEN": "t0ken",
		"KAIAK_INSTANCE_ID": "gw-1"}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range controlEnv {
			m[k] = v
		}
		for k, v := range extra {
			if v == "" {
				delete(m, k)
				continue
			}
			m[k] = v
		}
		return m
	}
	s, err := readSettings(envOf(controlEnv))
	if err != nil {
		t.Fatal(err)
	}
	if s.configFile != "" || s.control == nil || s.control.URL.String() != "https://cp.example:8443/base" ||
		s.control.Token != "t0ken" || s.control.BootWait != 60*time.Second {
		t.Errorf("settings %+v %+v", s, s.control)
	}
	s, err = readSettings(envOf(with(map[string]string{"KAIAK_CONTROL_BOOT_WAIT_MS": "250"})))
	if err != nil || s.control.BootWait != 250*time.Millisecond {
		t.Errorf("boot wait %v, %v", s.control, err)
	}

	for name, c := range map[string]struct {
		env  map[string]string
		want string
	}{
		"both modes":          {with(map[string]string{"KAIAK_CONFIG_FILE": "c.json"}), "both set"},
		"token only":          {with(map[string]string{"KAIAK_CONTROL_URL": ""}), "KAIAK_CONTROL_TOKEN is set without KAIAK_CONTROL_URL"},
		"token beside a file": {with(map[string]string{"KAIAK_CONTROL_URL": "", "KAIAK_CONFIG_FILE": "c.json"}), "KAIAK_CONTROL_TOKEN is set without"},
		"url only":            {with(map[string]string{"KAIAK_CONTROL_TOKEN": ""}), "KAIAK_CONTROL_URL is set without KAIAK_CONTROL_TOKEN"},
		"not a url":           {with(map[string]string{"KAIAK_CONTROL_URL": "cp.example:8443"}), "KAIAK_CONTROL_URL: want"},
		"query":               {with(map[string]string{"KAIAK_CONTROL_URL": "http://cp/?token=x"}), "KAIAK_CONTROL_URL: want"},
		"bad instance":        {with(map[string]string{"KAIAK_INSTANCE_ID": "gw 1"}), `instance ID "gw 1"`},
		"zero boot wait":      {with(map[string]string{"KAIAK_CONTROL_BOOT_WAIT_MS": "0"}), "KAIAK_CONTROL_BOOT_WAIT_MS"},
		"bad boot wait":       {with(map[string]string{"KAIAK_CONTROL_BOOT_WAIT_MS": "5s"}), "KAIAK_CONTROL_BOOT_WAIT_MS"},
	} {
		if _, err := readSettings(envOf(c.env)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want an error containing %q", name, err, c.want)
		}
	}
	// File mode does not send the instance ID anywhere that checks its shape.
	if _, err := readSettings(envOf(map[string]string{"KAIAK_CONFIG_FILE": "c.json", "KAIAK_INSTANCE_ID": "gw 1"})); err != nil {
		t.Errorf("file mode refused an instance ID: %v", err)
	}
}
