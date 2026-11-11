// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package i18n translates Pages-owned interface text, never stored page content
// or API/protocol values. Browser and server use the same embedded catalogs.
package i18n

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	pages "github.com/ElcanoTek/pages"
)

// Locale is a supported display language and an explicit URL preference. The
// latter follows navigation even when the browser cannot store preferences.
type Locale struct {
	Language string
	Explicit bool
	Location string
}

// Normalize maps supported language tags to their base language.
func Normalize(value string) string {
	tag := strings.ToLower(strings.TrimSpace(value))
	base, _, _ := strings.Cut(tag, "-")
	switch base {
	case "en", "es", "pt":
		return base
	}
	return ""
}

// First supplies English for helpers called without a request, including CLI
// fixtures. It never accepts an arbitrary value as an HTML language attribute.
func First(locales ...Locale) Locale {
	if len(locales) > 0 {
		if tag := Normalize(locales[0].Language); tag != "" {
			out := locales[0]
			out.Language = tag
			return out
		}
	}
	return Locale{Language: "en"}
}

type displayLocaleKey struct{}

// WithDisplayLocale carries a validated presentation choice on a copy of the
// request. Authentication and redirect handling keep the original URL and data.
func WithDisplayLocale(r *http.Request, locale Locale) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), displayLocaleKey{}, First(locale)))
}

// FromRequest prefers an explicit query, then the host-only preference cookie,
// then supported Accept-Language values in quality order, and finally English.
func FromRequest(r *http.Request) Locale {
	if r == nil {
		return First()
	}
	if locale, ok := r.Context().Value(displayLocaleKey{}).(Locale); ok {
		return locale
	}
	if lang := Normalize(r.URL.Query().Get("lang")); lang != "" {
		return Locale{Language: lang, Explicit: true, Location: r.URL.RequestURI()}
	}
	if c, err := r.Cookie("elcano_language"); err == nil {
		if lang := Normalize(c.Value); lang != "" {
			return Locale{Language: lang, Location: r.URL.RequestURI()}
		}
	}
	type preference struct {
		tag     string
		quality float64
	}
	var preferences []preference
	for _, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		fields := strings.Split(part, ";")
		q := 1.0
		for _, field := range fields[1:] {
			if key, value, ok := strings.Cut(strings.TrimSpace(field), "="); ok && key == "q" {
				v, err := strconv.ParseFloat(value, 64)
				if err != nil || v < 0 || v > 1 {
					q = 0
				} else {
					q = v
				}
			}
		}
		if lang := Normalize(fields[0]); lang != "" && q > 0 {
			preferences = append(preferences, preference{lang, q})
		}
	}
	sort.SliceStable(preferences, func(i, j int) bool { return preferences[i].quality > preferences[j].quality })
	if len(preferences) > 0 {
		return Locale{Language: preferences[0].tag, Location: r.URL.RequestURI()}
	}
	return Locale{Language: "en", Location: r.URL.RequestURI()}
}

var catalogs = loadCatalogs()

func loadCatalogs() map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, lang := range []string{"es", "pt"} {
		data, err := fs.ReadFile(pages.Public(), "shell-assets/i18n/"+lang+".json")
		if err != nil {
			panic(err)
		}
		var messages map[string]string
		if err = json.Unmarshal(data, &messages); err != nil {
			panic(err)
		}
		out[lang] = messages
	}
	return out
}

// T substitutes positional slots once, so values containing slot-like text are
// data and cannot become a second interpolation step.
func (l Locale) T(source string, values ...any) string {
	message := source
	if translated, ok := catalogs[First(l).Language][source]; ok {
		message = translated
	}
	var b strings.Builder
	for len(message) > 0 {
		start := strings.IndexByte(message, '{')
		if start < 0 {
			b.WriteString(message)
			break
		}
		b.WriteString(message[:start])
		message = message[start:]
		end := strings.IndexByte(message, '}')
		if end < 0 {
			b.WriteString(message)
			break
		}
		index, err := strconv.Atoi(message[1:end])
		if err == nil && index >= 0 && index < len(values) {
			_, _ = fmt.Fprint(&b, values[index])
		} else {
			b.WriteString(message[:end+1])
		}
		message = message[end+1:]
	}
	return b.String()
}

// URL carries an explicit language through first-party UI navigation. API
// endpoints are not passed here, and other query parameters stay intact.
func (l Locale) URL(path string) string {
	if !l.Explicit {
		return path
	}
	u, err := url.Parse(path)
	if err != nil || u.IsAbs() || u.Host != "" {
		return path
	}
	// Query parses and re-encodes unrelated values, and silently drops fields
	// with literal semicolons or malformed escapes. Only the owned key needs
	// decoding; preserve every other field, including its order and spelling.
	var fields []string
	if u.RawQuery != "" {
		for _, field := range strings.Split(u.RawQuery, "&") {
			key, _, _ := strings.Cut(field, "=")
			if decoded, decodeErr := url.QueryUnescape(key); decodeErr == nil && decoded == "lang" {
				continue
			}
			fields = append(fields, field)
		}
	}
	u.RawQuery = strings.Join(append(fields, "lang="+First(l).Language), "&")
	return u.String()
}

// Bootstrap returns safe JSON for a non-executable script island. json.Marshal
// escapes '<', preventing a catalog string from closing its containing element.
func (l Locale) Bootstrap() string {
	language := First(l).Language
	value := struct {
		Language string            `json:"language"`
		Messages map[string]string `json:"messages"`
	}{language, catalogs[language]}
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(data)
}

// Source marks a static message used through a dynamic lookup for the coverage
// check. It leaves the original protocol and stored values unchanged.
func Source(message string) string { return message }

// Script supplies a small translator inside a Pages-owned injected control.
// It never changes the surrounding user document or its language attribute.
func (l Locale) Script(sources ...string) string {
	messages := map[string]string{}
	for _, source := range sources {
		messages[source] = l.T(source)
	}
	data, err := json.Marshal(messages)
	if err != nil {
		panic(err)
	}
	language, _ := json.Marshal(First(l).Language)
	return "var messages=" + string(data) + ";var language=" + string(language) + ";var explicitLanguage=" + strconv.FormatBool(l.Explicit) + ";" +
		`function t(source){var values=Array.prototype.slice.call(arguments,1);return (Object.prototype.hasOwnProperty.call(messages,source)?messages[source]:source).replace(/\{(\d+)\}/g,function(slot,index){return Number(index)<values.length?String(values[Number(index)]):slot})}` +
		`function uiURL(href){try{var url=new URL(href,location.href);if(explicitLanguage&&url.origin===new URL(location.href).origin){url.searchParams.set("lang",language);return url.href}}catch(e){}return href}`
}
