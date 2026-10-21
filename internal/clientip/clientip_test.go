// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package clientip

import (
	"errors"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// Port of the IP-resolution parts of test/proxy.integration.js (the trust
// function per PAGES_TRUST_PROXY mode and the startup refusals); the
// end-to-end proxy hop through a limiter is in package ratelimit.

func TestTrustPerMode(t *testing.T) {
	for _, mode := range []string{"default", "direct", "custom"} {
		setting := map[string]string{"default": "", "direct": "false", "custom": "127.0.0.1/32,::1/128,198.51.100.0/24"}[mode]
		p, err := Parse(setting)
		if err != nil {
			t.Fatal(err)
		}
		for _, addr := range []string{"127.0.0.1", "::1", "::ffff:127.0.0.1"} {
			if got := p.Trusts(addr, 0); got != (mode != "direct") {
				t.Errorf("%s: Trusts(%s, 0) = %v", mode, addr, got)
			}
			if p.Trusts(addr, 1) {
				t.Errorf("%s: a second loopback hop is never trusted (%s)", mode, addr)
			}
		}
		if got := p.Trusts("198.51.100.9", 0); got != (mode == "custom") {
			t.Errorf("%s: explicit remote CIDR: %v", mode, got)
		}
		if p.Trusts("203.0.113.9", 0) {
			t.Errorf("%s: an unconfigured peer is not a proxy", mode)
		}
	}
}

func TestBooleansAndHopCountsFailAtStartup(t *testing.T) {
	for _, setting := range []string{"true", "1", "loopback,2", " TRUE ", "loopback, 10"} {
		if _, err := Parse(setting); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%q: %v", setting, err)
		}
	}
	if !strings.Contains(ErrUnsupported.Error(), "hop counts are unsupported") {
		t.Fatal(ErrUnsupported)
	}
	for setting, want := range map[string]string{
		"FALSE":                "invalid IP address: FALSE",
		"loopback,":            "invalid IP address: ",
		"10.0.0.0/0":           "invalid range on address: 10.0.0.0/0",
		"10.0.0.0/33":          "invalid range on address: 10.0.0.0/33",
		"10.0.0.0/255.0.255.0": "invalid range on address: 10.0.0.0/255.0.255.0",
		"::1/x":                "invalid range on address: ::1/x",
		"proxy.internal":       "invalid IP address: proxy.internal",
	} {
		if _, err := Parse(setting); err == nil || err.Error() != want {
			t.Errorf("%q: %v, want %q", setting, err, want)
		}
	}
	t.Setenv("PAGES_TRUST_PROXY", "2")
	if _, err := FromEnv(); !errors.Is(err, ErrUnsupported) {
		t.Errorf("FromEnv: %v", err)
	}
}

func TestNotations(t *testing.T) {
	p, err := Parse("uniquelocal, linklocal, 192.0.2.0/255.255.255.0, ::ffff:203.0.113.7, 2001:db8::/32, 0x7f.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	for addr, want := range map[string]bool{
		"10.1.2.3": true, "172.31.0.1": true, "172.32.0.1": false, "192.168.9.9": true, "fd00::1": true,
		"169.254.1.1": true, "fe80::1%eth0": true, "192.0.2.200": true, "192.0.3.1": false,
		"203.0.113.7": true, "::ffff:203.0.113.7": true, "2001:db8:1::1": true, "2001:db9::1": false,
		"127.0.0.1": true, "127.0.0.2": false, "not-an-ip": false, "": false,
	} {
		if got := p.Trusts(addr, 0); got != want {
			t.Errorf("Trusts(%q) = %v, want %v", addr, got, want)
		}
	}
	// An IPv4 peer matches an IPv6 range through its mapped form.
	p, _ = Parse("::ffff:0:0/96")
	if !p.Trusts("192.0.2.1", 0) {
		t.Error("IPv4 against an IPv4-mapped IPv6 range")
	}
}

func TestClientIP(t *testing.T) {
	loopback, _ := Parse("")
	direct, _ := Parse("false")
	cases := []struct {
		policy *Policy
		remote string
		xff    []string
		want   string
	}{
		{loopback, "127.0.0.1:5000", nil, "127.0.0.1"},
		{loopback, "127.0.0.1:5000", []string{"203.0.113.5"}, "203.0.113.5"},
		{loopback, "127.0.0.1:5000", []string{"192.0.2.10, 192.0.2.11, 203.0.113.5"}, "203.0.113.5"},
		{loopback, "127.0.0.1:5000", []string{"192.0.2.10", "203.0.113.5 "}, "203.0.113.5"},
		{loopback, "[::1]:5000", []string{"2001:db8::5"}, "2001:db8::5"},
		{loopback, "127.0.0.1:5000", []string{"garbage"}, "garbage"},
		{loopback, "127.0.0.1:5000", []string{" , ,"}, "127.0.0.1"},
		{loopback, "198.51.100.9:5000", []string{"203.0.113.5"}, "198.51.100.9"},
		{direct, "127.0.0.1:5000", []string{"203.0.113.5"}, "127.0.0.1"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		for _, v := range c.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		if got := c.policy.ClientIP(r); got != c.want {
			t.Errorf("%s %v: %q, want %q", c.remote, c.xff, got, c.want)
		}
	}
}

func TestForwardedForParsing(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Forwarded-For", " a ,b,, c d ,e ")
	if got := ForwardedFor(r); !slices.Equal(got, []string{"e", "c d", "b", "a"}) {
		t.Errorf("got %q", got)
	}
}
