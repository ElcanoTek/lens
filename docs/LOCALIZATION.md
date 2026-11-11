# Dashboard languages

Lens supports English, Spanish and Portuguese in its dashboard, file actions,
run monitor, custom-category editor, sign-out page and browser error messages.
Choose **English**, **Español** or **Português** in the header and apply the
selection. Portuguese uses European terminology.

An explicit `?lang=en|es|pt` takes precedence over the browser's saved choice,
then its preferred languages, then English. Regional tags such as `es-MX` and
`pt-BR` select the corresponding base language. Browser preferences use the
shared `elcano.language` localStorage key. A host-only, Secure, HttpOnly,
SameSite=Lax `elcano_language` cookie lets the server render subsequent pages
and action messages in the same language. Storage being disabled does not
prevent explicit links or the native language form from working.

Lens accepts Auth's `ui_locales` hint after `lang` and before the cookie. It
forwards `lang` on local/legacy sign-in navigation and `ui_locales` on central
Auth's authorization request. Locale selection never changes the callback URL,
login state, nonce, PKCE verifier, session cookies or CSRF validation. The
preference cookie is not shared across domains.

Owned UI messages live in `internal/i18n/locales/{es,pt}.json`, keyed by the
English source. Go templates use `tr`; owned browser scripts use `LensI18n.text`
and the HTML-escaping `LensI18n.html`. Use complete messages and explicit
singular/plural forms, preserve every `{0}` argument, and format presentation
numbers and dates for the selected locale. Catalogued diagnostics with printf
arguments are translated only at the browser boundary; captured file names and
model IDs remain opaque.

The CLI, machine API fields, persisted jobs/progress, output CSV headers,
taxonomy, user category definitions, provider model names and log contents
retain their original meaning and bytes. Language changes do not translate
customer data or alter model instructions. New UI strings require reviewed
Spanish and Portuguese translations. Run `make ci`; Go tests cover catalog
slots, language resolution, rendered pages, persistence and authentication
handoff. When Node is available, the tests also execute the shipped language
script to check preference priority and HTML escaping. Review dashboard,
category editor, monitor and sign-out interactions on mobile and desktop in all
three languages before changing their markup.
