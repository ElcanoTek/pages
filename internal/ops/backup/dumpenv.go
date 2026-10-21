// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package backup

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// dumpURLOptions are the URL options a backup reproduces exactly. Anything
// else, such as client certificate paths, is refused rather than dropped, so
// a capture can never quietly connect differently from the application.
var dumpURLOptions = []string{"host", "port", "user", "password", "sslmode"}

var postgresScheme = regexp.MustCompile(`(?i)^postgres(?:ql)?://`)

type connParts struct {
	host, port, user, password, database string
	hasPassword                          bool
}

// parseConnURL splits a postgres:// URL the way the application's driver
// (node-postgres' pg-connection-string, on WHATWG URL rules) did: the
// authority runs to the first '/', '?' or '#'; userinfo is everything before
// its LAST '@', split at the first ':'; each part is percent-decoded (an
// encoded '/' host is a socket directory); query host/port/user/password
// override the authority.
func parseConnURL(raw string) (connParts, url.Values, error) {
	var p connParts
	rest := raw[strings.Index(raw, "://")+3:]
	if i := strings.IndexByte(rest, '#'); i >= 0 {
		rest = rest[:i]
	}
	query := ""
	if i := strings.IndexByte(rest, '?'); i >= 0 {
		rest, query = rest[:i], rest[i+1:]
	}
	authority, path := rest, ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		authority, path = rest[:i], rest[i+1:]
	}
	hostport := authority
	if i := strings.LastIndexByte(authority, '@'); i >= 0 {
		userinfo := authority[:i]
		hostport = authority[i+1:]
		user, pass, has := strings.Cut(userinfo, ":")
		var err error
		if p.user, err = url.PathUnescape(user); err != nil {
			return p, nil, err
		}
		if has {
			if p.password, err = url.PathUnescape(pass); err != nil {
				return p, nil, err
			}
			p.hasPassword = true
		}
	}
	host := hostport
	if strings.HasPrefix(hostport, "[") {
		if j := strings.IndexByte(hostport, ']'); j >= 0 {
			host = hostport[:j+1]
			if rest := hostport[j+1:]; strings.HasPrefix(rest, ":") {
				p.port = rest[1:]
			}
		}
	} else if j := strings.LastIndexByte(hostport, ':'); j >= 0 {
		host, p.port = hostport[:j], hostport[j+1:]
	}
	var err error
	if p.host, err = url.PathUnescape(host); err != nil {
		return p, nil, err
	}
	if p.database, err = url.PathUnescape(path); err != nil {
		return p, nil, err
	}
	values, err := url.ParseQuery(query)
	if err != nil {
		return p, nil, err
	}
	if v := values.Get("host"); v != "" {
		p.host = v
	}
	if v := values.Get("port"); v != "" {
		p.port = v
	}
	if v := values.Get("user"); v != "" {
		p.user = v
	}
	if v, ok := values["password"]; ok && len(v) > 0 {
		p.password, p.hasPassword = v[len(v)-1], true
	}
	return p, values, nil
}

// queryKeys lists the option names of query in order, decoded, repeats kept.
func queryKeys(query string) []string {
	var keys []string
	for _, kv := range strings.Split(query, "&") {
		if kv == "" {
			continue
		}
		k, _, _ := strings.Cut(kv, "=")
		if d, err := url.QueryUnescape(k); err == nil {
			k = d
		}
		keys = append(keys, k)
	}
	return keys
}

// DumpEnvironment is the environment pg_dump connects with. libpq reads a
// connection URI only as a dbname argument, never from PGDATABASE, so the
// service's DATABASE_URL is parsed the way the application's driver parses
// it and handed over as PG* variables (never argv, which ps shows). The URL
// must be the only source (PG* alongside it are refused), and the driver's
// defaults stand in for parts the URL leaves out. Error messages never quote
// the URL or any part of it.
func DumpEnvironment(env map[string]string) (map[string]string, error) {
	result := map[string]string{}
	for k, v := range env {
		if k != "DATABASE_URL" {
			result[k] = v
		}
	}
	raw := env["DATABASE_URL"]
	if raw == "" {
		return result, nil
	}
	invalid := func(detail string) error {
		return errors.New("DATABASE_URL " + detail + "; backup needs the service's postgres:// URL")
	}
	if !postgresScheme.MatchString(raw) {
		return nil, invalid("is not a postgres:// connection URL")
	}
	// The driver also falls back to PG* variables (PGSSLMODE among them) for
	// whatever the URL leaves out. Rather than guess which wins, refuse both.
	for k := range env {
		if strings.HasPrefix(k, "PG") {
			return nil, invalid("is set together with PG* variables; keep the connection in DATABASE_URL alone")
		}
	}
	query := ""
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		query = strings.SplitN(raw[i+1:], "#", 2)[0]
	}
	// Option names are URL text, so they are never quoted back (they could
	// hold a pasted secret). A repeated option is refused: the parser takes
	// the last one, so validating any single copy could pass a different
	// effective value.
	seen := map[string]bool{}
	for _, k := range queryKeys(query) {
		allowed := false
		for _, a := range dumpURLOptions {
			allowed = allowed || a == k
		}
		if !allowed || seen[k] {
			return nil, invalid("has an unsupported or repeated option; backup accepts each of " + strings.Join(dumpURLOptions, ", ") + " at most once")
		}
		seen[k] = true
	}
	parts, values, err := parseConnURL(raw)
	if err != nil {
		return nil, invalid("could not be parsed")
	}
	sslmode, hasSSL := "", false
	if v, ok := values["sslmode"]; ok {
		sslmode, hasSSL = v[0], true
	}
	if hasSSL && sslmode != "disable" && sslmode != "verify-full" {
		return nil, invalid("sslmode must be disable or verify-full for backup")
	}
	// A decoded %00 can never reach a connection; refuse it here, without
	// the value.
	for _, part := range []string{parts.host, parts.port, parts.user, parts.password, parts.database} {
		if strings.ContainsRune(part, 0) {
			return nil, invalid("has a NUL character in a connection setting")
		}
	}
	host := parts.host
	if host == "" {
		host = "localhost"
	}
	result["PGHOST"] = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]") // libpq wants IPv6 bare
	if parts.port != "" {
		result["PGPORT"] = parts.port
	}
	if parts.user != "" {
		result["PGUSER"] = parts.user
	}
	if parts.password != "" {
		result["PGPASSWORD"] = parts.password
	}
	if parts.database != "" {
		result["PGDATABASE"] = parts.database
	}
	// node-postgres spoke no TLS unless asked, and verified fully when
	// asked. sslrootcert=system needs libpq 16+; an older libpq reads
	// "system" as a missing file and refuses to connect — fail closed.
	if sslmode == "verify-full" {
		result["PGSSLMODE"] = "verify-full"
		result["PGSSLROOTCERT"] = "system"
	} else {
		result["PGSSLMODE"] = "disable"
	}
	return result, nil
}
