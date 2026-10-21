// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package preflight

import (
	"strconv"
	"strings"

	"golang.org/x/net/idna"
)

// resolvedURL is the part of a WHATWG URL the subresource rule reads:
// url.protocol, url.host and url.origin.
type resolvedURL struct {
	protocol string // "https:", "data:", …
	host     string // hostname[:port], "" for opaque URLs
	origin   string // "https://example.com" for special schemes, "null" otherwise
}

var specialPorts = map[string]string{
	"http:": "80", "https:": "443", "ws:": "80", "wss:": "443", "ftp:": "21", "file:": "",
}

// resolveURL is `new URL(value, base)`, reduced to what the subresource rule
// needs. It follows the WHATWG URL parser for the parts that decide an origin —
// scheme detection, tab/newline removal, backslashes in special URLs,
// scheme-relative and path-relative references, userinfo, default ports,
// IPv6 literals, IDNA host mapping and forbidden host code points — and reports
// ok=false where the browser's parser throws. Paths, queries and fragments are
// never needed and are not normalised.
func resolveURL(value string, base resolvedURL) (resolvedURL, bool) {
	s := strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, strings.TrimFunc(value, func(r rune) bool { return r <= ' ' }))

	scheme, rest, hasScheme := splitScheme(s)
	if !hasScheme {
		if base.origin == "null" {
			return resolvedURL{}, false
		}
		return relative(s, base)
	}
	if _, special := specialPorts[scheme]; !special {
		// Opaque or non-special URL: the host is whatever follows "//", if any.
		u := resolvedURL{protocol: scheme, origin: "null"}
		if strings.HasPrefix(rest, "//") {
			host, ok := parseHost(authorityOf(rest[2:], false), scheme, false)
			if !ok {
				return resolvedURL{}, false
			}
			u.host = host
		}
		return u, true
	}
	rest = strings.ReplaceAll(rest, `\`, "/")
	if scheme == base.protocol && !strings.HasPrefix(rest, "/") {
		// "https:foo" against an https base is a relative reference.
		return relative(rest, base)
	}
	return withAuthority(scheme, strings.TrimLeft(rest, "/"))
}

func splitScheme(s string) (scheme, rest string, ok bool) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
		case i > 0 && (c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'):
		case i > 0 && c == ':':
			return strings.ToLower(s[:i+1]), s[i+1:], true
		default:
			return "", s, false
		}
	}
	return "", s, false
}

func relative(s string, base resolvedURL) (resolvedURL, bool) {
	s = strings.ReplaceAll(s, `\`, "/")
	if strings.HasPrefix(s, "//") {
		return withAuthority(base.protocol, strings.TrimLeft(s, "/"))
	}
	return base, true
}

// authorityOf cuts the authority off the front of what follows "//".
func authorityOf(s string, special bool) string {
	end := len(s)
	for i := 0; i < len(s); i++ {
		if c := s[i]; c == '/' || c == '?' || c == '#' || special && c == '\\' {
			end = i
			break
		}
	}
	return s[:end]
}

func withAuthority(scheme, s string) (resolvedURL, bool) {
	host, ok := parseHost(authorityOf(s, true), scheme, true)
	if !ok {
		return resolvedURL{}, false
	}
	if host == "" && scheme != "file:" {
		return resolvedURL{}, false
	}
	origin := scheme + "//" + host
	if scheme == "file:" {
		origin = "null"
	}
	return resolvedURL{protocol: scheme, host: host, origin: origin}, true
}

// parseHost turns an authority into WHATWG url.host.
func parseHost(authority, scheme string, special bool) (string, bool) {
	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		authority = authority[at+1:]
	}
	hostname, port := authority, ""
	if strings.HasPrefix(authority, "[") {
		end := strings.IndexByte(authority, ']')
		if end < 0 {
			return "", false
		}
		hostname, port = strings.ToLower(authority[:end+1]), authority[end+1:]
		if port != "" {
			if port[0] != ':' {
				return "", false
			}
			port = port[1:]
		}
	} else if i := strings.LastIndexByte(authority, ':'); i >= 0 {
		hostname, port = authority[:i], authority[i+1:]
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 0 || n > 65535 || strings.TrimLeft(port, "0123456789") != "" {
			return "", false
		}
		port = strconv.Itoa(n)
		if def, ok := specialPorts[scheme]; ok && def == port {
			port = ""
		}
	}
	if !strings.HasPrefix(hostname, "[") {
		if strings.ContainsAny(hostname, " #/:<>?@[\\]^|%\x00") {
			return "", false
		}
		if special {
			if hostname == "" {
				return "", false
			}
			ascii, err := idna.Lookup.ToASCII(hostname)
			if err != nil {
				return "", false
			}
			hostname = ascii
		}
	}
	if port != "" {
		return hostname + ":" + port, true
	}
	return hostname, true
}
