# Pages — what actually works inside a published page

The contract for anyone (human or agent) writing the HTML Pages serves. Every
rule here is enforced by a real response header, and every one of them has
already broken a live client dashboard at least once.

`preflight_page` checks a page against this document automatically, and every
`deploy_page` / `update_page` / `deploy_page_upload` result carries the same
findings in its `preflight` field. **Read that field.** It is the only thing in
the loop that can actually see whether your page works — the authoring sandbox
has no browser, and the live page is not anonymously fetchable.

Subresource checks resolve relative, protocol-relative (`//host/path`) and
absolute URLs against the configured content origin, then use the same
per-resource CSP directives as rendered pages. An absolute URL on that origin
is permitted where `'self'` is allowed. Scheme support depends on the resource:
for example, blob scripts and data/blob images are permitted, data scripts are
blocked, and media permits embedded data/blob URLs but not same-origin files.
These findings describe CSP permission, not whether an asset exists or has the
right MIME type; they do not change the served policy.

## The one that keeps happening: inline handlers do not see your globals

```html
<!-- BROKEN. Silently. -->
<button onclick="togglePopover()">Date range</button>
<script>function togglePopover(){ /* never runs */ }</script>
```

An inline `on*=` handler is compiled with the **element**, its **form owner**,
and the **document** pushed onto its scope chain ahead of `window`. A bare
identifier resolves against those first. `togglePopover` is a real method on
`HTMLElement` (the Popover API), so that button calls the *built-in*, throws
`NotSupportedError`, and does nothing. Your function is never reached.

This shipped to a live campaign dashboard and survived five deploy attempts and
two user complaints, because nothing in the authoring loop could execute a page.

**506 names are shadowed this way** — including `close`, `open`, `select`,
`submit`, `reset`, `clear`, `search`, `focus`, `blur`, `title`, `value`, `name`,
`scroll`, `download`, `showPopover` and `hidePopover`. All the obvious ones.

Do this instead:

```html
<button id="drTrigger">Date range</button>
<script>
  document.getElementById('drTrigger').addEventListener('click', togglePopover);
</script>
```

`addEventListener` closes over normal lexical scope and has no shadowing
problem. If you must keep an inline handler, qualify the call
(`onclick="window.togglePopover()"`) or prefix the global (`drTogglePopover`).

## What the content host allows

Pages are served from a separate, cookieless registrable domain under
`sandbox allow-scripts allow-downloads allow-modals` plus a strict CSP
(`lib/csp.js` is the source of truth). In practice:

| You want to | Works? | Notes |
|---|---|---|
| Inline `<script>` / `<style>` | ✅ | `'unsafe-inline'` is granted deliberately — charts need it |
| Inline SVG, canvas, hand-rolled charts | ✅ | The normal way to draw here |
| `data:` images and `data:` `@font-face` | ✅ | How you ship a brand logo or typeface |
| `blob:` images | ✅ | For charts that rasterise before drawing |
| `blob:` **scripts** — `import()` a module or start a Worker from bytes the page already holds | ✅ | Granted for documents that ship their runtime compressed (a Bento deck). `connect-src 'none'` still applies: a blob is built, never fetched |
| Embedded `data:`/`blob:` video and audio | ✅ | `media-src data: blob:`. Remote media is a beacon and stays blocked |
| Trigger a CSV/Excel download | ✅ | `allow-downloads` |
| `window.print()` for a PDF button | ✅ | `allow-modals` |
| Link to another page **in the same tab** | ✅ | `<a href="/other-slug">` or `location.assign(...)`. A sandboxed top-level document may navigate *itself*; `allow-top-navigation` governs a framed document navigating its parent, which is not this |
| Load **anything** from a remote host | ❌ | No CDNs, no Google Fonts, no hotlinked logos. Inline it |
| `fetch` / `XHR` / WebSocket | ❌ | `connect-src 'none'`. Embed the data at deploy time |
| `localStorage`, `sessionStorage`, cookies | ❌ **throws** | Opaque origin → `SecurityError` |
| `window.open`, or a link with `target="_blank"` | ❌ | `allow-popups` is withheld. The click does nothing at all — no tab, no error |
| Submit a form | ❌ | `form-action 'none'` |

The two navigation rows are measured, not inferred: `test/browser/sandbox-nav.spec.js`
drives real Chromium against the real `rawHeaders()` and asserts both — that a
same-tab link and `location.assign` reach the sibling, that `target="_blank"`
does not, and that the viewer's session cookie survives the hop so they are not
asked for a password again. **Chromium only** — `playwright.config.js` declares
no other project, and Firefox/WebKit are not installed in CI. Sandbox and
cookie semantics are exactly the area where engines differ, so check Safari by
hand before a navigation-dependent page goes to an outside viewer.

Two of these fail in a way that is much worse than "doesn't work":

- **Remote subresources fail silently.** A hotlinked client logo just renders
  as a broken image. Inline it as a `data:` URI, and add
  `onerror="this.style.display='none'"` so a miss degrades cleanly.
- **Storage APIs `throw`.** An unguarded `localStorage.getItem()` at the top
  level of your script takes down *everything after it in that script*. If you
  genuinely need it, wrap every access in `try/catch`; better, keep the state in
  a module-level variable. A dashboard has no reason to persist across loads.

## Bento decks

A Bento deck — the `.bento.html` that fleet's `bento-slides` skill produces — is
one self-contained file that is its own viewer **and editor**: a 650KB runtime,
deflate-compressed, that a bootstrap inflates and `import()`s from a `blob:` URL,
around one JSON block of slides. Deploy the file as it is. Pages recognises the
`<script type="application/bento+json">` block and does three things:

- **Serves it raw, with one addition.** `render_mode` defaults to `raw` for a deck
  and `themed` is refused: a deck carries a complete design of its own. In a portal
  it gets no Page menu either — the deck's own toolbar is exactly where the control
  would land. The one tag Pages adds (`<script data-pages-deck-host>`) removes the
  native file-picker API before the deck boots, because the sandbox refuses every
  picker ("Sandboxed documents aren't allowed to show a file picker") and Bento
  chooses its Save path by whether that API exists — without this, Save on Chrome
  and Edge did nothing at all. It is stripped again on upload.
- **Lets it boot.** `script-src` grants `blob:` for this. Without it a deck dies
  with *"This file could not start"* while preflight, which reads plaintext, calls
  it clean.
- **Says so in preflight.** A deck reports one `bento_deck` finding that explains
  what follows, because the static rules cannot see inside the runtime.

What a reader gets is Bento's full editor, opened on the deck. On a served page
that means:

- **Nothing edited is saved to Pages.** *Save* downloads a new `.bento.html` in
  every browser — Bento labels the button "download an updated copy… this browser
  can't rewrite the open file" — and *Save as…* offers a copy, a duplicate, an
  encrypted copy. *Add image* and *Replace from JSON* open an ordinary file
  chooser. To change the deck people see,
  save the edited file and make it the next version — from the page's *Edit
  source* dialog (**Upload a file**), or by having an agent stage it with
  `create_upload_ticket` and call `deploy_page_upload`. Either way it lands as a
  draft for review, like every other version.
- **Unsaved edits are gone on reload.** The sandbox has no storage, so Bento's
  in-browser backup is off; Bento warns before the tab closes.
- **Export PDF works** (`window.print()`, `allow-modals`). **Speaker view does
  not** — it needs a pop-up, and Bento says so in its own words when asked. Nor
  do the copy-to-clipboard actions (*Copy document JSON*): clipboard write needs
  a permission the sandbox does not grant.
- **A file saved from the hosted editor carries live-collaboration keys.** Bento
  mints a `collab` block (room URL, private key) into any file saved from its UI.
  Inert here — both CSPs forbid connections — but preflight warns
  (`bento_deck_collab_keys`) when such a file is deployed, because it is key
  material in a shareable document. Round-tripping it through fleet's
  `bento_doc.py` (`get`, `set`) drops the block.
- **Size.** A deck is ~690KB before any content. The inline body cap and the
  upload-ticket cap are both 2MB, so a deck with embedded images needs the upload
  path and has to stay under it. Embedded video must be `data:` or `blob:`.

`readonly: true` in the deck's document makes a **player** file that boots
straight into the presentation with no editor — set it on a hand-out copy.

### Saving back to Pages: the edit session

Everything above is what a *reader* of the deck gets. Staff get one more thing:
**Edit in Bento** on the page's detail screen opens the deck in an **edit
session** — the newest version of the page (drafts included), served for a
signed, eight-hour, page-bound token that names who opened it. In that session:

- **Save saves to Pages.** Every one of Bento's save entry points (the button,
  ⌘S, *Save a copy…*) is intercepted and the serialised deck is posted to Pages,
  where it lands as a new **draft** version attributed to the staff member. The
  deck says so in a small toast; you publish from the admin, as with any draft.
  If Pages cannot be reached or refuses the save, Pages starts a download from
  the exact bytes and filename captured at Save, even if Bento has already
  released its original download URL. Confirm your browser kept that file
  before closing the editor, then upload it as a new version. If the download
  cannot start, the message tells you to keep the editor open and try Save again.
- **The channel is the token, not the sandbox.** The session's CSP opens
  `connect-src` to Pages' own origin and nothing else (`rawEditHeaders()`); the
  deck's own guard is widened the same way for that response only, and restored
  in the stored bytes on every save. Readers, partners and clients never get this
  response — their deck stays `connect-src 'none'` and Save downloads.
- **Only a deck can be saved through it**, only to the page the token names, and
  only as a draft. The `collab` block Bento mints on save is removed before
  storage (`lib/bento.js`), so the stored version carries no keys.
- **Saves follow the latest draft.** Each request names the version the editor
  started from in `X-Pages-Base-Version`. Saves from one tab run in order and
  advance that base only when acknowledged. If another editor or source update
  has saved a newer version, Pages refuses the stale save without adding a
  draft. Keep the fallback file, reopen **Edit in Bento**, and reconcile the
  changes before saving again. Queued saves from the old tab also fall back to
  files; they never adopt the conflicting version automatically. Repeating a
  save whose exact contents are already the latest version creates no duplicate.
  A page requiring approval still receives a pending version for review.

Agents do not use this channel — `deploy_page_upload` is theirs, and it is the
same state machine underneath.

The boot mechanism is pinned by `test/browser/bento-deck.spec.js` with a
synthetic deck. `node test/manual/bento-deck-check.js Deck.bento.html` runs the
same probes against a real one — run it when Bento is re-vendored in fleet.

## Giving a partner a menu of their other dashboards

A partner who reaches a dashboard through a **portal** — one link and one password
over a set of dashboards — gets a `Page ▾` menu of the others that credential
opens. **You do not have to build this.** Pages injects the list into `<head>` at
render time, and injects a built-in control to render it unless your document
already references `#pages-nav`, in which case the menu is yours to draw. So the
rules below apply only when you want to replace the built-in one:

```html
<script type="application/json" id="pages-nav" data-flag-injected>
{"portal":{"slug":"nwm","name":"Northwind Media Group"},
 "pages":[{"slug":"nwm-contoso","title":"Contoso — Allergex",
           "url":"https://elcano-pages.com/nwm-contoso","current":true}],
 "truncated":false}
</script>
```

Five rules, four of which are the difference between a working menu and a broken
dashboard:

1. **Guard, then render nothing.** The block is absent on a staff view, on a
   page-password view, and on any page in no portal. An unguarded
   `JSON.parse(document.getElementById("pages-nav").textContent)` is a `TypeError`
   that takes down *everything after it in that script* — the same failure mode as
   the storage APIs above, and the most likely way to break a page with this.
2. **Use the `url` as given.** It is absolute and ready to use. Slugs nest, so on
   `/a/b/c` a relative `href="d"` resolves to `/a/b/d`, and `base-uri 'none'` means
   a `<base>` tag cannot rescue it.
3. **Real `<a href>` elements, never `window.open` or a scripted popup.**
   `allow-popups` is withheld: a `target="_blank"` click does nothing at all, with
   no error. Anchors also give you keyboard and middle-click behaviour for free. A
   native `<details>`/`<summary>` gives you the open/close with no script.
4. **`textContent`, never `innerHTML`, for a title.** A sibling's title is set by
   whoever owns that page. The payload's JSON cannot break out of the block, but
   `innerHTML = title` would execute markup of someone else's choosing inside your
   dashboard.
5. **`raw` gets the menu too, and no theming.** A raw page in a portal receives the
   payload and (unless it reads the block itself) the built-in control — never Flag
   tokens, fonts or the theme controller. What `raw` protects is "do not restyle my
   design", and that is intact; navigation is not styling. With no portal it is
   still byte-for-byte. A Bento deck is the one exception: it is byte-for-byte in
   a portal too — see *Bento decks* below.

`templates/nwm-campaign-dashboard/template.html` carries a working implementation
of all five, and `test/browser/page-switcher.spec.js` drives it in a real browser.

## Structure every page so it can be reused

Not because it has to be reused — most pages never are — but because the page you
are proudest of is the one someone will ask you to do again for another client,
and the cost of being ready for that is now approximately zero.

Three script blocks and two lines of glue:

```html
<!-- what differs per instance: identity, naming, brand, thresholds -->
<script type="application/json" id="pages-config">
{"client":"Acme Corp","brand":{"primary":"#0a3d62"},"kpiTarget":3.5}
</script>

<!-- what the refresh replaces -->
<script type="application/schema+json" id="pages-data-schema">{ … }</script>
<script type="application/json" id="pages-data">
{"contract_version":1,"refreshed_at":"…","source_as_of":"…","data":{"rows":[]}}
</script>

<script>
  const CONFIG = JSON.parse(document.getElementById('pages-config').textContent);
  const DATA   = JSON.parse(document.getElementById('pages-data').textContent).data;

  // Read EVERY per-instance value from CONFIG — including colours.
  document.documentElement.style.setProperty('--brand', CONFIG.brand.primary);
  document.getElementById('client').textContent = CONFIG.client;
</script>
```

**You do not write a config schema.** Ship `#pages-config` alone and Pages derives
`#pages-config-schema` from your values on deploy, writing it into the stored page
and telling you it did (`config_schema_generated: true`). The derived schema pins
types and rejects unknown keys — enough to catch a typo'd key or a number sent as
a string. It does not know your enums or which keys are genuinely optional, so
replace it by hand if the design becomes a family. A key whose value is `null` is
inferred **optional**, which is the idiom for "not set yet".

Then, when someone asks for the same thing for another client:

```
create_template_from_page  slug="acme-q3"  template="campaign-dashboard"
                           empty_data={"rows":[]}
create_page_from_template  template="campaign-dashboard"  slug="globex-q3"
                           config={"client":"Globex", …}
```

The design never moves. It is already on the server.

**The one thing to get right while authoring:** anything per-instance must be read
from `CONFIG`, not written into the markup as well. A `<title>Acme Corp</title>`
alongside `CONFIG.client` means every page built from the design says Acme.
`create_template_from_page` and `validate_template` both report those as
`hardcoded_config_values`, but it is cheaper to not write them twice.

## Before you write it: is this a family?

If the page is another instance of a design Pages already has — another campaign,
another client, the same dashboard with different numbers — **do not write the
HTML at all**:

```
list_templates                                            → what designs exist
get_template               template="nwm-campaign-dashboard"  → its config + data schemas, no HTML
create_page_from_template  template="nwm-campaign-dashboard" slug="…" config={…}
```

That costs the config that actually differs — kilobytes — instead of the whole
document, and it keeps the page attached to the design, so a later fix can reach
it. See **[TEMPLATES.md](TEMPLATES.md)**. Everything below applies to a design
you are genuinely authoring for the first time.

When you *are* authoring a new design that others will reuse, give it a
`#pages-data-example` block: a fictional dataset that satisfies its own data
schema. It is preview-only — deleted from every page built from the template — and
without it the library can only show your design's empty state, which is not
enough to judge it by. Keep both it and the reference config fictional; everyone
who opens the library sees them.

## Getting a large document in

Every byte of inline `html` and every base64 chunk is output **the model
generates token by token**, so size is a direct multiplier on cost and on the
chance a turn dies mid-deploy.

- **≤ 20,000 bytes:** `deploy_page` with inline `html`.
- **Anything larger, if your shell can reach the network:**
  `create_upload_ticket` → one `curl -X PUT --data-binary @file` →
  `deploy_page_upload`. **The bytes never enter your context**, so a 300 KB
  dashboard costs the same as a 30 KB one.
- **Larger, with no outbound HTTP:** `start_page_upload` → `append_page_upload`
  → `deploy_page_upload`. Chunks may be up to the `max_chunk_bytes` the start
  call returns (**49,152** by default — a 65 KB dashboard is two calls).

`max_chunk_bytes` is a ceiling, not a quota: smaller chunks are always fine and
sequence numbers do not depend on size.

**Never shrink a working dashboard to fit the transport.** Dropping rows,
columns or date granularity to save chunks trades away the thing the client
asked for. If an upload is failing, cancel it and start a new one — the upload
tools exist so the document does not have to change.

If you hit the active-upload cap, uploads that have been idle for an hour are
reaped automatically on your next `start_page_upload`. Uploads still in flight
are never touched; `cancel_page_upload` is still the right call for one you know
you have abandoned.

## Changing one thing

Do not re-upload a whole dashboard to fix a CSS rule or rename a function.

```
find_in_version  slug="contoso-allergex" query="ctl-group{"     → line + excerpt
patch_page       slug="contoso-allergex"
                 edits=[{find:".ctl-group{display:flex;",
                         replace:".ctl-group{position:relative;display:flex;"}]
```

`find` is a **literal** string (not a regex) and must match exactly `count`
times — default 1. If it does not, the whole patch is rejected and nothing is
deployed, because a patch that quietly did nothing is how a "fixed" dashboard
ships unfixed. Widen the anchor until it is unique, or pass an explicit `count`
when every occurrence should change.

The result is a normal immutable version with the usual `preflight` field, and
the concurrency check defaults to the version the patch was computed from — so a
patch cannot land on top of a deploy you never saw.

## Before you tell the user it is done

Inline classic scripts and `type="module"` scripts are syntax-checked in their
respective grammars without running code or resolving imports. Modules support
imports, exports and top-level `await`; a syntax pass does not prove referenced
resources exist or that the browser will allow them. Each script retains the
1 MiB analysis limit.

1. Check the `preflight` field on your deploy result. `ok: false` means part of
   the page does not work — fix it and redeploy before sharing the link.
2. Share `urls.live` verbatim.
3. A page with no password is Elcano-staff-only. To share with a client, call
   `set_password` first, then send the URL and the password.

Preflight is advisory and never blocks a deploy: a false positive must not be
able to wedge an agent, and humans own publish. But an error is almost always
real, and it is real *in the browser the client is about to open*.
