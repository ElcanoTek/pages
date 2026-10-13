# Flag Motion — live dashboards and motion explainers

Flag Motion is a small first-party kit Pages serves from its own content host. A
page opts in with two tags and gets what people now expect from AI-built
dashboards and animated explainers:

```html
<link rel="stylesheet" href="/assets/flag/motion/flag-motion.css">
<script src="/assets/flag/motion/flag-motion.js" defer></script>
```

Nothing is fetched at view time. The content sandbox forbids network access
(`connect-src 'none'`), and the kit needs none: it reads the page's own managed
blocks. It is opt-in, so no existing page changes until its HTML includes it.

| You want | Write | What the viewer gets |
| --- | --- | --- |
| A number that counts up | `<b data-fm-count="1500" data-fm-format="usd">` | Counts from 0 when scrolled into view, lands on `$1,500.00` exactly |
| Sections that ease in | `data-fm-reveal`, or `data-fm-stagger` on a parent | Fade-up on scroll; children staggered |
| "How fresh is this?" | `<span data-fm-freshness></span>` | `● Updated 3 h ago · data through Oct 11, 2026`, from `#pages-data`'s `refreshed_at` / `source_as_of` |
| "Where does this number come from?" | `data-fm-metric="spend"` on the number | Click or Enter opens what it is, the formula, the source file, the scope and the refresh cadence |
| A chart | `<div data-fm-chart='{…spec…}'>` or `data-fm-chart="data:charts.trend"` | Themed SVG, drawn in, hover tooltip, keyboard reading, a *Show data* table |
| An animated explainer | `<div data-fm-explainer='{…scenes…}'>` | A canvas player: autoplay on view, play/pause, scrub, scene chips, **Download video** (MP4, or WebM where the browser cannot record MP4) |

All of it respects `prefers-reduced-motion` (everything settles at once and
nothing autoplays), prints settled values, and keeps content visible if the
script fails, because elements are hidden only after it has run.

## Live: specs bound to managed data

A chart or explainer spec can live in the page's data instead of its HTML:

```html
<div data-fm-chart="data:charts.dailySpend"></div>
<div data-fm-explainer="data:explainer"></div>
```

`data:a.b` reads `#pages-data` → `data.a.b`, `config:a.b` reads `#pages-config`,
and `#some-id` reads any `<script type="application/json" id="some-id">`. Put the
specs in the data payload and every scheduled refresh redraws the charts and
re-cuts the explainer with the new numbers. Nothing in the HTML changes, so the
refresh stays a data-only update. This is how a Pages dashboard stays live: the
data is refreshed on a schedule, not queried at view time.

## Provenance

Every `data-fm-metric` number opens a card built from, in order:

1. `<script type="application/json" id="pages-provenance">`, or a `provenance`
   key inside `#pages-config`:

   ```json
   {
     "metrics": {
       "spend": {
         "label": "SSP spend",
         "definition": "What buyers spent on this campaign's deals, as the SSPs report it.",
         "formula": "Σ Spend ($) over in-scope deals",
         "source": "ssp",
         "filter": "Deals tagged EXAMPLE-CAMPAIGN"
       }
     },
     "sources": {
       "ssp": { "label": "Example SSP report", "location": "Shared reports · ssp_history.csv", "cadence": "As configured by the source owner" }
     }
   }
   ```

2. Otherwise, the `description` of the property with that name in
   `#pages-data-schema`, so a schema-described page explains its numbers with
   no extra work.

The card always ends with the page's freshness line. Write `location` for the
audience: an admin-only page can name the file, while a client-shareable page
should name the report, not the internal path. State the source's real cadence
and time zone; there is no default.

## Chart spec

```json
{ "type": "line", "title": "Daily spend", "format": "usd",
  "labels": ["2026-01-01", "2026-01-02"],
  "series": [{ "name": "SSP A", "values": [100, 110] }, { "name": "SSP B", "values": [50, null] }],
  "target": 120, "targetLabel": "Goal", "source": "Example SSP report" }
```

| Key | Types | Meaning |
| --- | --- | --- |
| `type` | — | `line`, `area`, `bar` (vertical, grouped), `hbar` (ranked list), `donut`, `spark` |
| `labels` + `series[]` | line, area, bar, spark | Categories (ISO days print as `Oct 6`) and up to **six** series. `null` is a gap, never a zero |
| `items[]` | hbar, donut | `{ "label", "value" }`. A donut folds past six slices into *Other*; `hbar` shows `limit` rows (default 10) and counts the rest |
| `format` | all | `usd`, `usd0`, `usdc` ($1.2M), `int`, `num`, `num1`, `compact`, `pct`/`pct2` (fractions), `x` (multiples) |
| `target` | line, area, bar | A dashed reference line with a label |
| `source` | all | A footer line: `Source: … · data through <source_as_of>` |
| `freshness: true` | all | Also print *Updated … ago* under this chart |
| `colorBy: "item"` | hbar | One colour per entity, keyed to the order the items are listed (never their rank) |
| `table: false` | all | Drop the *Show data* table, which is on by default |

Colours come from a categorical palette validated for colour-blind separation
on Flag's light and dark surfaces (`--fm-s1` … `--fm-s6`). Series take slots in
the order given and never cycle. Text never wears a series colour. Override any
`--fm-*` variable to restyle a page.

## Explainer spec (animation as code)

All names, identifiers and figures in these examples are invented.

```json
{ "title": "Example campaign in 30 seconds", "brand": "Northwind Media",
  "source": "Example SSP report", "filename": "example-recap", "aspect": "16:9",
  "scenes": [
    { "type": "title",   "kicker": "Campaign recap", "title": "Example campaign — ten days", "subtitle": "Display across two example deals" },
    { "type": "kpis",    "title": "What it delivered", "items": [{ "label": "SSP spend", "value": 1500, "format": "usd0", "delta": 0.1 }] },
    { "type": "bars",    "title": "Top deals", "format": "usd0", "items": [{ "label": "example-deal-a", "value": 1000 }] },
    { "type": "line",    "title": "Daily spend", "format": "usd0", "labels": ["2026-01-01", "…"], "series": [{ "name": "Spend", "values": [150, "…"] }],
                         "annotate": { "index": 4, "text": "Example: promotion starts" } },
    { "type": "donut",   "title": "Spend by SSP", "format": "usd0", "items": [{ "label": "SSP A", "value": 900 }] },
    { "type": "compare", "title": "Week over week", "format": "usd0", "a": { "label": "Last week", "value": 600 }, "b": { "label": "This week", "value": 900 } },
    { "type": "bullets", "title": "What to do next", "items": ["Shift budget toward example-deal-a"] },
    { "type": "closing", "title": "Questions? Ask your account team." }
  ] }
```

- Every word, number and `duration` (ms) is editable. Change one and the rest
  stays as it was. Default durations: title 3.8 s, kpis 5.2 s, bars 6.2 s,
  line 6.4 s, donut 5.6 s, bullets 5.6 s, compare 5 s, closing 4.2 s.
- `aspect`: `16:9` (1920×1080), `1:1`, `4:5` or `9:16` for social. `theme`:
  `"dark"` (default, Elcano plum) or `"light"`, or an object overriding
  `bg1 bg2 fg muted accent good bad series[]`.
- KPI `delta` is a fraction (`0.18` → `▲ 18.0% vs prior`). It is green when
  up, unless the item sets `"higherIsBetter": false`. The arrow and the sign
  carry the meaning, so it never relies on colour alone.
- **Download video** records the timeline in real time (a 40-second
  explainer takes 40 seconds) and saves `filename.mp4`, or `.webm` where the
  browser cannot record MP4. The button is hidden where recording is not
  possible.
- The player renders a hidden transcript of every scene for screen readers and
  agents, and the canvas reads `Space` to play or pause and arrows to move
  between scenes.

## Script API

`window.FlagMotion` is there for pages that compute specs from `DATA`:

```js
const c = FlagMotion.chart(el, spec);        // → { update(spec), destroy() }
const x = FlagMotion.explainer(el, spec);    // → { play(), pause(), seek(ms), exportVideo(), state() }
FlagMotion.count(el, 1234.5, { format: "usd" });
FlagMotion.format(0.1234, "pct1");           // "12.3%"
FlagMotion.freshness();                      // { state, text, updated, through }
FlagMotion.init(container);                  // wire data-fm-* added after load
```

A page that builds its DOM in its own script calls `FlagMotion.init(root)`
afterwards. Calling it again is harmless.
