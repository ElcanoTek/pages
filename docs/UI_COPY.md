# The words the admin uses

Four screens were built in four sessions and each picked its own words for the same
things. A reader moving between them met **page** and **dashboard** for one object,
**Open as staff** and **staff-only** and **Elcano-only** for one access tier, and
sentences explaining the architecture where they expected to be told what a control
does. That is the "messy" in the audit (#141, #157) more than any single screen is.

This file is the decision. It is short on purpose: if a word is not here, use the
ordinary one.

## Nouns — one per concept

| Concept | Say | Not |
|---|---|---|
| A hosted client page | **page** | dashboard, client page (for one item) |
| An immutable snapshot of a page | **version** | revision |
| A stored design pages are built from | **template**, and **revision** for its snapshots | design |
| A one-level group of pages | **workspace** | folder, group |
| A partner's set of pages behind one credential | **portal** | |

`page` wins over `dashboard` because it is what the database, the admin API and
three of the four screens already said; only the portals screen disagreed. The
content host is the exception, and deliberately so — see below.

## Verbs — one per operation

| Say | Means |
|---|---|
| **New X** | create a new top-level object (New page, New portal) |
| **Add X** | attach something that already exists (Add a page to a portal) |
| **Review** | look at a version and decide |
| **Publish** | make a version the one clients see |
| **Roll back** | make an earlier version the one clients see |
| **Disable** | reversible takedown; the page stays, clients get an error |
| **Delete** | remove the object |
| **Remove** | detach from a set, leaving the object alone |
| **Retire** | take out of service, keeping history and freeing the name |

## Access

**staff-only** is the term, everywhere. Not *Elcano-only*, not *staff*. A control
that opens the page a client sees is named for that destination — **View live** —
not for who is pressing it.

## Sentences

- A screen's intro is **one line**, and it says what you do here. The reasoning
  belongs in docs or a help affordance, not over the `<h1>`.
- A table caption states **facts** — a count, a scope. If it needs to explain
  something, that explanation is a `.field-help` next to the control it is about.
- No jargon in operator-facing copy. Not *lossless*, *lifecycle state*, *moves the
  live pointer*, *injects Flag foundations*. Say what happens to the reader's page:
  "Clients will see version 6 instead of version 2."
- Sentence case. No trailing period on a label or a button.
- Say what a thing does, not that you are sure: "Approve and publish version 6?"
  beats "Are you sure?".

## The content host is a different audience

Everything above is for operators. `lib/contentview.js`, `lib/render.js` and the
templates speak to a **partner**, and the rule there is one line: *no word a
partner would not use*.

- **dashboard**, not page. It is their word for the thing, and on that host there
  is no admin vocabulary to collide with.
- **Start here** marks the page a portal opens with, on the index and in the Page
  menu alike. It replaced an `Overview` tag that repeated a word most of those
  pages already had in their title.
- Never a status code (`404`), a lifecycle state (`unpublished`, `disabled`), a
  piece of our infrastructure (`this host`, `rendered`, `sandbox`, `payload`), or
  the mechanics of a credential (`signed`, `short-lived`). Say when it stopped
  working and who can fix it.
- A timestamp says which claim it is making: **Data as of** for the data itself,
  **Updated** for when we last published. They are not interchangeable.
- One deliberate exception: the staff line on a protected page addresses Elcano
  staff by name, so it may name the dashboard they should open instead.

Unlike the rest of this file, that rule *is* enforced — `test/unit.test.js`
renders every partner-facing page and fails on the words above, and pins the two
page switchers to the same strings.

## Where this is enforced

Nowhere automatically — it is a review checklist, not a test. The one piece that
*is* enforced is the design system: `test/theme.test.js` fails the build on a
colour literal or an unresolvable token. Copy is still a human call.
