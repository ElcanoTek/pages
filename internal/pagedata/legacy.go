// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pagedata

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/jsonv"
)

// Earlier revisions of the shipped nwm-campaign-dashboard template wrote
// DATA.lastRefreshed into the page header with innerHTML, and their schema
// accepted any string. The template is fixed, but a page built from an
// earlier revision keeps that revision's source (stored page versions are
// append-only, and Pages never rewrites a client's markup), so a data
// refresh could still carry markup into it. Until such a page is rebuilt
// from the current revision, every data write to a page whose layout still
// has that sink is refused if lastRefreshed could form markup. Plain
// refresh stamps ("Aug 2, 2026", ISO timestamps) are unaffected.
var legacyStampSink = regexp.MustCompile(`\.innerHTML\s*=\s*CONFIG\.lastRefreshed\s*\?`)

// The same revisions built the header's KPI note by concatenating
// CONFIG.channels[].name and kpiLabel into innerHTML unescaped, so config
// writes to a page whose layout still has that sink refuse a label with "<",
// the only character that lets HTML text open an element. "&" and ">" stay
// allowed: labels such as "Display (O&O)" are real, and in text they can at
// most spell an entity, never markup.
var legacyKpiSink = regexp.MustCompile(`CONFIG\.channels\.map\(\s*c\s*=>\s*c\.name\s*\+`)

// markupChars are what HTML text needs to open a tag or an entity.
const markupChars = "<>&"

// assertLegacyStampSafe applies that guard to proposed data for m.
func assertLegacyStampSafe(m *Managed, data any) error {
	o, ok := data.(*jsonv.Object)
	if !ok {
		return nil
	}
	stamp, ok := o.Value("lastRefreshed").(string)
	if !ok || !strings.ContainsAny(stamp, markupChars) || !legacyStampSink.MatchString(m.HTML) {
		return nil
	}
	return apierror.BadRequest("data does not satisfy the page's JSON Schema", "data_validation_failed", ValidationDetails{
		ValidationErrors: []BoundedError{{
			InstancePath: "/lastRefreshed", SchemaPath: "#/properties/lastRefreshed", Keyword: "pattern",
			Message: "must be plain text: this page's layout writes it as HTML, so <, > and & are refused; rebuild the page from the current template revision",
		}},
		TotalErrors: 1,
	})
}

// assertLegacyChannelLabelsSafe applies the KPI-note guard to a proposed
// config for m.
func assertLegacyChannelLabelsSafe(m *Managed, config any) error {
	o, ok := config.(*jsonv.Object)
	if !ok {
		return nil
	}
	channels, _ := o.Value("channels").([]any)
	var bad []BoundedError
	for i, ch := range channels {
		co, ok := ch.(*jsonv.Object)
		if !ok {
			continue
		}
		for _, field := range []string{"name", "kpiLabel"} {
			if v, ok := co.Value(field).(string); ok && strings.Contains(v, "<") {
				bad = append(bad, BoundedError{
					InstancePath: "/channels/" + strconv.Itoa(i) + "/" + field,
					SchemaPath:   "#/properties/channels/items/properties/" + field,
					Keyword:      "pattern",
					Message:      "must be plain text: this page's layout writes it as HTML, so < is refused; rebuild the page from the current template revision",
				})
			}
		}
	}
	if len(bad) == 0 || !legacyKpiSink.MatchString(m.HTML) {
		return nil
	}
	total := len(bad)
	if len(bad) > 12 {
		bad = bad[:12]
	}
	return apierror.BadRequest("config does not satisfy the page's JSON Schema", "config_validation_failed",
		ValidationDetails{ValidationErrors: bad, TotalErrors: total})
}
