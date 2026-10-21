// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package psl computes registrable domains (eTLD+1) from the vendored Mozilla
// Public Suffix List. The DASHBOARD_HOST/CONTENT_HOST trust split is a
// security boundary (PLAN.md §7): two hostnames on one registrable domain let
// agent HTML toss cookies onto the trusted host. The comparison uses the FULL
// list, not a shortlist: a shortlist mis-reduces omitted suffixes (com.ng
// absent → pages.contoso.com.ng and pages.northwind.com.ng both reduce to
// 'com.ng' and produce a false shared-domain failure). See the dat file's
// header for source, license (MPL-2.0) and refresh cadence.
package psl

import (
	_ "embed"
	"strings"
	"sync"
)

//go:embed public-suffix-list.dat
var list string

type rules struct {
	normal    map[string]bool // "co.uk"
	wildcard  map[string]bool // "*.ck" stored as "ck"
	exception map[string]bool // "!www.ck" stored as "www.ck"
}

var (
	once   sync.Once
	parsed rules
)

func load() rules {
	once.Do(func() {
		parsed = rules{normal: map[string]bool{}, wildcard: map[string]bool{}, exception: map[string]bool{}}
		for _, line := range strings.Split(list, "\n") {
			line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
			if line == "" || strings.HasPrefix(line, "//") || strings.HasPrefix(line, "#") {
				continue // blanks, Mozilla comments, our vendoring header
			}
			switch {
			case strings.HasPrefix(line, "!"):
				parsed.exception[line[1:]] = true
			case strings.HasPrefix(line, "*."):
				parsed.wildcard[line[2:]] = true
			default:
				parsed.normal[line] = true
			}
		}
	})
	return parsed
}

// RegistrableDomain returns the eTLD+1 of host per the Public Suffix List
// algorithm (https://publicsuffix.org/list/): the prevailing rule is the
// longest matching rule (a wildcard matches exactly one label, an exception
// rule drops its leftmost label), the public suffix is the labels that match,
// and the registrable domain is the suffix plus one more label to its left.
// It lowercases and tolerates a trailing dot. A host that is itself inside a
// public suffix is returned whole. "" is returned for an empty host.
func RegistrableDomain(host string) string {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" {
		return ""
	}
	r := load()
	labels := strings.Split(host, ".")
	n := len(labels)
	best, bestException := 0, false
	for k := 1; k <= n; k++ {
		suffix := strings.Join(labels[n-k:], ".")
		parent := strings.Join(labels[n-k+1:], ".")
		match, exception := false, false
		if r.normal[suffix] || (k > 1 && r.wildcard[parent]) {
			match = true
		}
		if r.exception[suffix] {
			match, exception = true, true
		}
		// Longest match prevails; on a TIE an exception rule wins over a
		// wildcard of equal length — for foo.www.ck both '*.ck' and '!www.ck'
		// match at length 2, and the exception is what makes www.ck a normal
		// registrable domain (subdomains of it share one cookie boundary).
		if match && (k > best || (k == best && exception && !bestException)) {
			best, bestException = k, exception
		}
	}
	suffixLabels := best
	switch {
	case best == 0:
		suffixLabels = 1 // the implicit "*" rule
	case bestException:
		suffixLabels = max(best-1, 1)
	}
	reg := suffixLabels + 1
	if reg > n {
		return host
	}
	return strings.Join(labels[n-reg:], ".")
}

// SameSite reports whether a and b share a registrable domain.
func SameSite(a, b string) bool {
	ra, rb := RegistrableDomain(a), RegistrableDomain(b)
	return ra != "" && ra == rb
}
