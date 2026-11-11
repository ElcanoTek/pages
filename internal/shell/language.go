// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package shell

import (
	"strings"

	"github.com/ElcanoTek/pages/internal/i18n"
)

var languageNames = []struct{ tag, name string }{{"en", "English"}, {"es", "Español"}, {"pt", "Português"}}

// LanguagePicker uses native language names so a visitor can recover from an
// unfamiliar choice. Browser preferences remain host-local and optional.
func LanguagePicker(locale i18n.Locale) string {
	var b strings.Builder
	b.WriteString(`<label class="sr-only" for="pages-language">` + Esc(locale.T("Language")) + `</label><select id="pages-language" class="language-picker" aria-label="` + Esc(locale.T("Language")) + `">`)
	for _, item := range languageNames {
		selected := ""
		if item.tag == locale.Language {
			selected = ` selected`
		}
		b.WriteString(`<option value="` + item.tag + `"` + selected + `>` + item.name + `</option>`)
	}
	b.WriteString(`</select>`)
	return b.String()
}

// LanguageLinks offers the same choice on scriptless access and error pages.
// Each link keeps the current path and query, including signed link parameters.
func LanguageLinks(locale i18n.Locale) string {
	var b strings.Builder
	location := locale.Location
	if strings.HasPrefix(location, "//") {
		location = "/" + strings.TrimLeft(location, "/")
	}
	b.WriteString(`<nav class="language-links" aria-label="` + Esc(locale.T("Language")) + `">`)
	for _, item := range languageNames {
		choice := locale
		choice.Language = item.tag
		choice.Explicit = true
		current := ""
		if item.tag == locale.Language {
			current = ` aria-current="true"`
		}
		b.WriteString(`<a href="` + Esc(choice.URL(location)) + `" lang="` + item.tag + `"` + current + `>` + item.name + `</a>`)
	}
	b.WriteString(`</nav>`)
	return b.String()
}
