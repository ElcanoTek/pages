# Interface language

Pages supports English, Spanish, and Portuguese. Choose **Language** in the dashboard header. The page reloads with the selected language and keeps the current URL parameters and fragment, including the selected workspace or portal. Password prompts, portal indexes, and error pages offer language links that work without JavaScript.

An explicit `lang=en`, `lang=es`, or `lang=pt` query parameter takes precedence. The dashboard then uses a saved browser preference, a host-only preference cookie, the browser's supported languages, or English. Spanish and Portuguese regional browser tags select the corresponding language; Portuguese interface copy uses European Portuguese vocabulary.

Choosing a language stores `elcano.language` in local storage and `elcano_language` in a host-only, SameSite=Lax cookie. HTTPS uses Secure cookies. If the browser denies either storage mechanism, the language in the URL still works and follows Pages navigation. Scriptless pages use the URL, their host's cookie if present, and the request's browser-language preferences.

Language changes affect Pages controls and guidance. They do not translate customer page content, titles, workspace names, portal names, HTML source, identifiers, API values, or logs. The small Pages navigation and save controls embedded in published pages translate independently of the surrounding customer document.

## Maintaining translations

`public/shell-assets/i18n/es.json` and `pt.json` are source-keyed catalogs shared by Go and browser code. English text is the source. Use `locale.T("Message {0}", value)` in Go and `t("Message {0}", value)` in browser modules. Interpolation runs once; values remain data. Escape rendered HTML with the existing shell helpers and use DOM text nodes in browser code.

Keep complete sentences and separate singular/plural forms. Preserve numeric placeholders, code, URLs, commands, and identifiers. Mark a static Go message that is selected dynamically with `i18n.Source` so catalog coverage can find it. Never apply translation to arbitrary user content or HTML.

Server language links and authentication return URLs replace only the decoded `lang` query key. Keep all other raw query fields, including literal semicolons, repeated keys, empty fields, and opaque percent escapes; parsing and re-encoding the entire query can silently discard data. Authentication still validates local return paths and applies its existing fragment and HTTP redirect escaping rules.

`go test ./internal/i18n` checks request negotiation and server-message coverage. `node --test test/frontend.test.js` checks browser negotiation, storage denial, catalog parity, placeholder parity, and browser-message coverage. `npx playwright test test/browser/localization.spec.js` covers all dashboard sections, scriptless gates, storage failures, language persistence, and unchanged customer content.
