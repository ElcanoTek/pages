// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package mcpproto

// Host and origin normalization as lib/mcp.js did it with the WHATWG URL
// parser (`new URL("http://" + host).hostname`, `new URL(origin).origin`), for
// the ASCII inputs a Host or Origin header can carry: lower-cased names,
// default ports dropped, IPv4 shorthand (127.1, 0x7f.0.0.1) and IPv6 text
// canonicalized. Non-ASCII (IDNA) hostnames are refused rather than
// punycoded; they never name a Pages host.

import (
	"errors"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"github.com/ElcanoTek/pages/internal/jsstr"
)

var originShape = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*://[^/?#]+$`)

// NormalizeOrigin is lib/mcp.js normalizeOrigin: the serialized origin of an
// http(s) URL with no path, query, fragment or credentials, else "".
func NormalizeOrigin(value string) string {
	candidate := jsTrim(value)
	if !originShape.MatchString(candidate) {
		return ""
	}
	// The URL parser drops ASCII tab and newline anywhere.
	candidate = strings.NewReplacer("\t", "", "\n", "", "\r", "").Replace(candidate)
	scheme, rest, _ := strings.Cut(candidate, "://")
	scheme = strings.ToLower(scheme)
	if scheme != "http" && scheme != "https" {
		return ""
	}
	// Special schemes treat a backslash as a path separator.
	authority, _, _ := strings.Cut(rest, `\`)
	host, ok := parseAuthority(authority, scheme)
	if !ok {
		return ""
	}
	return scheme + "://" + host
}

// NormalizeHostname is lib/mcp.js normalizeHostname: the URL hostname of
// "http://<value>", lower-cased, or "" when value is not a bare host[:port].
func NormalizeHostname(value string) string {
	candidate := strings.ToLower(jsTrim(value))
	return hostnameOf(candidate)
}

// RequestHostname is the hostname of a Host header value, or "".
func RequestHostname(host string) string { return hostnameOf(host) }

func hostnameOf(candidate string) string {
	if candidate == "" || strings.ContainsAny(candidate, "@/?#\\") || strings.IndexFunc(candidate, jsstr.IsSpace) >= 0 {
		return ""
	}
	hostport, ok := parseAuthority(candidate, "http")
	if !ok {
		return ""
	}
	// hostname excludes the port; IPv6 keeps its brackets.
	if strings.HasPrefix(hostport, "[") {
		end := strings.IndexByte(hostport, ']')
		return hostport[:end+1]
	}
	h, _, _ := strings.Cut(hostport, ":")
	return h
}

// parseAuthority parses userinfo@host:port for a special scheme and returns
// the serialized host[:port] (default port dropped). Credentials refuse it.
func parseAuthority(authority, scheme string) (string, bool) {
	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		// new URL keeps an empty username and password: only "" and ":"
		// leave both empty.
		if user := authority[:at]; user != "" && user != ":" {
			return "", false
		}
		authority = authority[at+1:]
	}
	var hostText, portText string
	hasPort := false
	if strings.HasPrefix(authority, "[") {
		end := strings.IndexByte(authority, ']')
		if end < 0 {
			return "", false
		}
		hostText = authority[:end+1]
		rest := authority[end+1:]
		if rest != "" {
			if rest[0] != ':' {
				return "", false
			}
			portText, hasPort = rest[1:], true
		}
	} else {
		colon := strings.IndexByte(authority, ':')
		if colon >= 0 {
			hostText, portText, hasPort = authority[:colon], authority[colon+1:], true
		} else {
			hostText = authority
		}
	}
	host, ok := parseHost(hostText)
	if !ok {
		return "", false
	}
	if hasPort && portText != "" {
		for _, c := range portText {
			if c < '0' || c > '9' {
				return "", false
			}
		}
		trimmed := strings.TrimLeft(portText, "0")
		if len(trimmed) > 5 {
			return "", false
		}
		port := 0
		if trimmed != "" {
			port, _ = strconv.Atoi(trimmed)
		}
		if port > 65535 {
			return "", false
		}
		if (scheme != "http" || port != 80) && (scheme != "https" || port != 443) {
			host += ":" + strconv.Itoa(port)
		}
	}
	return host, true
}

func parseHost(input string) (string, bool) {
	if input == "" {
		return "", false
	}
	if strings.HasPrefix(input, "[") {
		if !strings.HasSuffix(input, "]") {
			return "", false
		}
		addr, err := netip.ParseAddr(input[1 : len(input)-1])
		if err != nil || !addr.Is6() || addr.Zone() != "" {
			return "", false
		}
		return "[" + ipv6Text(addr) + "]", true
	}
	domain, ok := percentDecode(input)
	if !ok {
		return "", false
	}
	for i := 0; i < len(domain); i++ {
		c := domain[i]
		if c >= 0x80 {
			return "", false // IDNA is not supported: refuse
		}
		if c <= 0x20 || c == 0x7F || strings.IndexByte("#%/:<>?@[\\]^|", c) >= 0 {
			return "", false
		}
	}
	domain = strings.ToLower(domain)
	if endsInNumber(domain) {
		return parseIPv4(domain)
	}
	return domain, true
}

func percentDecode(s string) (string, bool) {
	if !strings.Contains(s, "%") {
		return s, true
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if h, ok1 := hexVal(s[i+1]); ok1 {
				if l, ok2 := hexVal(s[i+2]); ok2 {
					b.WriteByte(h<<4 | l)
					i += 2
					continue
				}
			}
		}
		b.WriteByte(s[i])
	}
	return b.String(), true
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

func ipv4Parts(s string) []string {
	parts := strings.Split(s, ".")
	if len(parts) > 1 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// endsInNumber is the WHATWG "ends in a number" checker.
func endsInNumber(s string) bool {
	parts := ipv4Parts(s)
	last := parts[len(parts)-1]
	if last != "" && strings.Trim(last, "0123456789") == "" {
		return true
	}
	if len(last) >= 2 && (last[:2] == "0x" || last[:2] == "0X") {
		return strings.Trim(last[2:], "0123456789abcdefABCDEF") == ""
	}
	return false
}

func parseIPv4Number(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	base := 10
	switch {
	case len(s) >= 2 && (s[:2] == "0x" || s[:2] == "0X"):
		s, base = s[2:], 16
	case len(s) >= 2 && s[0] == '0':
		s, base = s[1:], 8
	}
	if s == "" {
		return 0, true
	}
	n, err := strconv.ParseUint(s, base, 64)
	if err != nil {
		// Overflowing digits are still a (too large) number.
		if errors.Is(err, strconv.ErrRange) {
			return 1 << 40, true
		}
		return 0, false
	}
	return n, true
}

// parseIPv4 is the WHATWG IPv4 parser; it serializes dotted decimal.
func parseIPv4(s string) (string, bool) {
	parts := ipv4Parts(s)
	if len(parts) > 4 {
		return "", false
	}
	nums := make([]uint64, len(parts))
	for i, p := range parts {
		n, ok := parseIPv4Number(p)
		if !ok {
			return "", false
		}
		nums[i] = n
	}
	for _, n := range nums[:len(nums)-1] {
		if n > 255 {
			return "", false
		}
	}
	last := nums[len(nums)-1]
	if last >= 1<<(8*(5-len(nums))) {
		return "", false
	}
	ipv4 := last
	for i, n := range nums[:len(nums)-1] {
		ipv4 += n << (8 * (3 - i))
	}
	return strconv.FormatUint(ipv4>>24, 10) + "." + strconv.FormatUint(ipv4>>16&0xFF, 10) + "." +
		strconv.FormatUint(ipv4>>8&0xFF, 10) + "." + strconv.FormatUint(ipv4&0xFF, 10), true
}

// ipv6Text is the WHATWG/RFC 5952 serialization: lower-case hex, no leading
// zeros, the first longest run of two or more zero pieces as "::", and never
// a dotted-quad tail.
func ipv6Text(addr netip.Addr) string {
	raw := addr.As16()
	var words [8]uint16
	for i := range words {
		words[i] = uint16(raw[2*i])<<8 | uint16(raw[2*i+1])
	}
	best, bestLen := -1, 1
	for i := 0; i < 8; {
		if words[i] != 0 {
			i++
			continue
		}
		j := i
		for j < 8 && words[j] == 0 {
			j++
		}
		if j-i > bestLen {
			best, bestLen = i, j-i
		}
		i = j
	}
	hex := func(ws []uint16) string {
		s := make([]string, len(ws))
		for i, w := range ws {
			s[i] = strconv.FormatUint(uint64(w), 16)
		}
		return strings.Join(s, ":")
	}
	if best < 0 {
		return hex(words[:])
	}
	return hex(words[:best]) + "::" + hex(words[best+bestLen:])
}

// jsTrim is String#trim over a header value. Header bytes are Latin-1 to
// Node, so a raw 0xA0 byte is the NBSP code point and trims.
func jsTrim(s string) string {
	isSpaceByte := func(c byte) bool {
		switch c {
		case '\t', '\n', '\v', '\f', '\r', ' ', 0xA0:
			return true
		}
		return false
	}
	start, end := 0, len(s)
	for start < end && isSpaceByte(s[start]) {
		start++
	}
	for end > start && isSpaceByte(s[end-1]) {
		end--
	}
	return s[start:end]
}
