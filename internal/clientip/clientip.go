// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package clientip resolves the client address of a request under the
// PAGES_TRUST_PROXY policy — Express's "trust proxy" as server.js configured
// it.
//
// Caddy is one local hop in the supported deployment. The setting names the
// proxy peers (IPs, CIDRs, IPv4 netmask notation, or the names "loopback",
// "linklocal", "uniquelocal"; comma-separated; default "loopback"), and trust
// is bounded to that immediate hop: when the connecting peer is a configured
// proxy, the client is the rightmost X-Forwarded-For entry it appended, and
// earlier forwarded values are never used — even a loopback client behind
// Caddy must not make them trusted. "false" turns forwarding off for direct
// access with no proxy. Blanket booleans and hop counts ("true", "2") are
// refused rather than interpreted as proxy addresses.
package clientip

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"github.com/ElcanoTek/pages/internal/env"
)

// ErrUnsupported is the startup refusal of a boolean or hop-count setting.
var ErrUnsupported = errors.New(`PAGES_TRUST_PROXY must contain proxy IPs/CIDRs, "loopback", or "false"; booleans and hop counts are unsupported`)

// The named ranges proxy-addr expands.
var namedRanges = map[string][]string{
	"linklocal":   {"169.254.0.0/16", "fe80::/10"},
	"loopback":    {"127.0.0.1/8", "::1/128"},
	"uniquelocal": {"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"},
}

// Policy is a compiled PAGES_TRUST_PROXY setting.
type Policy struct {
	disabled bool
	subnets  []netip.Prefix
}

var hopCount = regexp.MustCompile(`(?i)^(?:true|\d+)$`)

// Parse compiles a PAGES_TRUST_PROXY value ("" means the default
// "loopback"). Invalid entries fail as Express failed at startup.
func Parse(setting string) (*Policy, error) {
	peers := strings.TrimSpace(setting)
	if peers == "" {
		peers = "loopback"
	}
	for _, peer := range strings.Split(peers, ",") {
		if hopCount.MatchString(strings.TrimSpace(peer)) {
			return nil, ErrUnsupported
		}
	}
	if peers == "false" {
		return &Policy{disabled: true}, nil
	}
	p := &Policy{}
	for _, entry := range strings.Split(peers, ",") {
		entry = strings.TrimSpace(entry)
		notes := []string{entry}
		if named, ok := namedRanges[entry]; ok {
			notes = named
		}
		for _, note := range notes {
			prefix, err := parseNotation(note)
			if err != nil {
				return nil, err
			}
			p.subnets = append(p.subnets, prefix)
		}
	}
	return p, nil
}

// FromEnv is Parse(PAGES_TRUST_PROXY).
func FromEnv() (*Policy, error) { return Parse(env.String("PAGES_TRUST_PROXY", "")) }

// MustFromEnv is FromEnv for server startup.
func MustFromEnv() *Policy {
	p, err := FromEnv()
	if err != nil {
		panic(err)
	}
	return p
}

// parseNotation is proxy-addr's parseipNotation: ip, ip/bits or
// ipv4/netmask. A bare IPv4-mapped IPv6 address is stored as IPv4.
func parseNotation(note string) (netip.Prefix, error) {
	str, rangeText, hasRange := note, "", false
	if pos := strings.LastIndexByte(note, '/'); pos >= 0 {
		str, rangeText, hasRange = note[:pos], note[pos+1:], true
	}
	ip, ok := parseIP(str)
	if !ok {
		return netip.Prefix{}, fmt.Errorf("invalid IP address: %s", str)
	}
	if !hasRange && ip.Is4In6() {
		ip = ip.Unmap()
	}
	bitsMax := ip.BitLen()
	bits := bitsMax
	if hasRange {
		bits = -1
		switch {
		case rangeText != "" && strings.Trim(rangeText, "0123456789") == "":
			if n, err := strconv.Atoi(rangeText); err == nil {
				bits = n
			} else {
				bits = bitsMax + 1
			}
		case ip.Is4():
			if mask, ok := parseIP(rangeText); ok && mask.Is4() {
				bits = prefixFromMask(mask)
			}
		}
	}
	if bits <= 0 || bits > bitsMax {
		return netip.Prefix{}, fmt.Errorf("invalid range on address: %s", note)
	}
	return netip.PrefixFrom(ip, bits).Masked(), nil
}

// prefixFromMask is ipaddr.js prefixLengthFromSubnetMask: -1 (invalid) unless
// the mask is contiguous ones then zeros.
func prefixFromMask(mask netip.Addr) int {
	b := mask.As4()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	n := 0
	for n < 32 && v&(1<<(31-n)) != 0 {
		n++
	}
	if v<<n != 0 {
		return -1
	}
	return n
}

var ipv4Part = `(0?\d+|0x[a-f0-9]+)`
var (
	fourOctet = regexp.MustCompile(`(?i)^` + ipv4Part + `\.` + ipv4Part + `\.` + ipv4Part + `\.` + ipv4Part + `$`)
	longValue = regexp.MustCompile(`(?i)^` + ipv4Part + `$`)
)

// parseIntAuto is ipaddr.js 1.9.1's: a leading "0" (not "0x") means octal,
// and like parseInt it stops at the first digit the radix does not have.
func parseIntAuto(s string) uint64 {
	base := 10
	switch {
	case len(s) > 1 && s[0] == '0' && s[1] == 'x':
		s, base = s[2:], 16
	case s != "" && s[0] == '0':
		base = 8
	}
	var n uint64
	for i := 0; i < len(s); i++ {
		c := s[i]
		var d int
		switch {
		case c >= '0' && c <= '9':
			d = int(c - '0')
		case c >= 'a' && c <= 'f':
			d = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = int(c-'A') + 10
		default:
			return n
		}
		if d >= base {
			return n
		}
		n = n*uint64(base) + uint64(d)
		if n > 1<<40 {
			return n
		}
	}
	return n
}

// parseIP is ipaddr.js 1.9.1's isValid + parse: IPv4 in four parts (each
// decimal, 0-octal or 0x-hex) or as one 32-bit number, or IPv6 (a zone is
// accepted and ignored).
func parseIP(s string) (netip.Addr, bool) {
	if m := fourOctet.FindStringSubmatch(s); m != nil {
		var b [4]byte
		for i, part := range m[1:5] {
			n := parseIntAuto(part)
			if n > 255 {
				return netip.Addr{}, false
			}
			b[i] = byte(n)
		}
		return netip.AddrFrom4(b), true
	}
	if m := longValue.FindStringSubmatch(s); m != nil {
		n := parseIntAuto(m[1])
		if n > 0xFFFFFFFF {
			return netip.Addr{}, false
		}
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(n))
		return netip.AddrFrom4(b), true
	}
	if !strings.Contains(s, ":") {
		return netip.Addr{}, false
	}
	addr, err := netip.ParseAddr(s)
	if err != nil || !addr.Is6() {
		return netip.Addr{}, false
	}
	return addr.WithZone(""), true
}

// Disabled reports whether forwarding is off ("false").
func (p *Policy) Disabled() bool { return p.disabled }

// Trusts is the "trust proxy fn": whether addr at the given hop (0 = the
// connecting peer) is a trusted proxy. Only hop 0 can ever be trusted.
func (p *Policy) Trusts(addr string, hop int) bool {
	if p.disabled || hop != 0 {
		return false
	}
	ip, ok := parseIP(addr)
	if !ok {
		return false
	}
	for _, subnet := range p.subnets {
		candidate := ip
		if subnet.Addr().Is4() != ip.Is4() {
			if subnet.Addr().Is4() {
				if !ip.Is4In6() {
					continue
				}
				candidate = ip.Unmap()
			} else {
				candidate = netip.AddrFrom16(ip.As16())
			}
		}
		if subnet.Contains(candidate) {
			return true
		}
	}
	return false
}

// RemoteAddr is the connecting peer's address without the port, as Node's
// socket.remoteAddress gives it (IPv6 unbracketed). Go reports an IPv4 peer
// of a dual-stack listener as plain IPv4 where Node said ::ffff:a.b.c.d.
func RemoteAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ForwardedFor parses X-Forwarded-For as the forwarded package did: the
// comma-separated entries right to left (nearest proxy's first), trimmed of
// spaces, empty entries skipped. Repeated headers are joined first, as Node
// joins them.
func ForwardedFor(r *http.Request) []string {
	header := strings.Join(r.Header.Values("X-Forwarded-For"), ", ")
	var list []string
	end, start := len(header), len(header)
	for i := len(header) - 1; i >= 0; i-- {
		switch header[i] {
		case ' ':
			if start == end {
				start, end = i, i
			}
		case ',':
			if start != end {
				list = append(list, header[start:end])
			}
			start, end = i, i
		default:
			start = i
		}
	}
	if start != end {
		list = append(list, header[start:end])
	}
	return list
}

// ClientIP is req.ip: the rightmost X-Forwarded-For entry when the peer is a
// trusted proxy, otherwise the peer itself. The entry is returned as sent,
// unvalidated, as Express did.
func (p *Policy) ClientIP(r *http.Request) string {
	addrs := append([]string{RemoteAddr(r)}, ForwardedFor(r)...)
	for i := 0; i < len(addrs)-1; i++ {
		if !p.Trusts(addrs[i], i) {
			return addrs[i]
		}
	}
	return addrs[len(addrs)-1]
}
