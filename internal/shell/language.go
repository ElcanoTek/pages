// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package shell

import (
	"encoding/json"
	"html/template"
	"strings"

	"github.com/ElcanoTek/pages/internal/i18n"
)

var languageNames = []struct{ tag, name string }{{"en", "English"}, {"es", "Español"}, {"pt", "Português"}}

var languageBootstrapTemplate = template.Must(template.New("language-bootstrap").Parse(`<script type="application/json" id="pages-i18n">{{.}}</script>`))

// languageBootstrap renders JSON in a script-data context through html/template,
// so quotes remain JSON syntax and '<' cannot close the containing element.
func languageBootstrap(locale i18n.Locale) string {
	var b strings.Builder
	if err := languageBootstrapTemplate.Execute(&b, json.RawMessage(locale.Bootstrap())); err != nil {
		panic(err)
	}
	return b.String()
}

// LanguagePicker uses native language names so a visitor can recover from an
// unfamiliar choice. Browser preferences remain host-local and optional.
func LanguagePicker(locale i18n.Locale) string {
	var b strings.Builder
	b.WriteString(`<label class="sr-only" for="pages-language">` + Esc(locale.T("Language")) + `</label><span class="select-wrap select-wrap--sm language-picker-wrap"><select id="pages-language" class="language-picker" aria-label="` + Esc(locale.T("Language")) + `">`)
	for _, item := range languageNames {
		selected := ""
		if item.tag == locale.Language {
			selected = ` selected`
		}
		b.WriteString(`<option value="` + item.tag + `"` + selected + `>` + item.name + `</option>`)
	}
	b.WriteString(`</select>` + Icon("chevron-down", "select-icon") + `</span>`)
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
