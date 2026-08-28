# Licensing

Pages is **source available**, not open source. This page explains what that
means in plain English. The controlling document is [`LICENSE`](../LICENSE) —
where this summary and the licence disagree, the licence wins.

## The short version

| | |
| --- | --- |
| Licence | Business Source License 1.1 (BUSL-1.1) |
| Licensor | ElcanoTek, Inc. |
| Additional Use Grant | **None** — no production use without a commercial licence |
| Change Licence | MIT |
| Change Date | Two years after the version was first published |
| Commercial licensing | licensing@elcanotek.com |

You can read the code, run it, modify it, and share your changes. You cannot
run it in production for real work unless you buy a commercial licence — or
unless the copy you are using has already passed its Change Date, at which
point it is MIT and none of this applies to it any more.

## What you may do today, for free

- **Read, study and audit the source.** All of it, including the security model.
- **Clone, fork and modify it.** Derivative works are explicitly permitted.
- **Redistribute it**, original or modified, as long as the licence travels with
  it. Every copy must display `LICENSE` conspicuously.
- **Run it for any non-production purpose**: local development, evaluation,
  testing, CI, security research, a demo you show a colleague, a course you
  teach, a proof of concept you throw away.
- **Contribute back.** See [`CONTRIBUTING.md`](../CONTRIBUTING.md).

## What you may not do without a commercial licence

- **Run it in production.** The Additional Use Grant is `None`, so the licence
  grants you no production use at all — not even a small amount, not even
  internally, not even for free.
- **Offer it as a service.** Hosting Pages for anyone else — customers,
  clients, another business unit — is production use.
- **Strip or alter the licence.** BSL 1.1 covenant 4 forbids modifying the
  licence text, and removing it terminates your rights.

Using it outside these terms automatically terminates your rights **for every
version**, not just the one you misused. If you need production rights, ask —
that is what the commercial licence is for.

## What "non-production" means here

There is no bright line in the licence text, so read it the way the words
suggest: production use is use that your organisation, or anyone else, actually
depends on. The practical test is whether a failure would matter to someone
other than you.

Non-production, unambiguously:

- A developer running `scripts/dev.sh` on a laptop.
- A CI job that boots Pages, runs the suite, and tears it down.
- An internal spike to decide whether to buy a licence.
- A security researcher probing the two-host trust split.

Production, unambiguously:

- Serving a real dashboard to a real client, paid or unpaid.
- A deployment your team relies on for internal reporting.
- Anything with a real DNS name, real data and real users.

If you are unsure which side you are on, you are probably close enough to the
line that a short email to licensing@elcanotek.com is cheaper than guessing.

## How the rolling two-year Change Date works

This is the part people most often get wrong, so it is worth being precise.

BSL applies **per version**. Each version of Pages carries its own Change Date,
and on that date **that version** — and only that version — converts to the
Change Licence, MIT.

For this repository, the Change Date of a given version is **two years after
the author date of the commit that produced it**. So:

- Every commit starts a fresh two-year clock **for the version it produces**.
- A version already published keeps the Change Date it was published with. A
  later commit cannot push it back. It converts to MIT on schedule whether or
  not the project is still maintained.
- The copy in your working tree converts two years after *its* commit date, not
  two years after you downloaded it and not two years after the latest commit
  on `main`.

Once a version converts, it is MIT forever. You may use that version in
production, and so may anyone you gave it to. What you may **not** do is treat
a newer, still-BSL version as if it inherited the older version's MIT status —
each version stands alone.

To compute the Change Date for the exact copy you are holding:

```bash
./scripts/bsl-change-date.sh          # for HEAD
./scripts/bsl-change-date.sh <ref>    # for any commit, tag or branch
```

It prints the commit, its author date, the resulting Change Date, and the
Change Licence. It reads Git history, so it needs to run inside a clone (a
release tarball with no `.git` cannot answer the question — check the commit it
was cut from).

## The four-year cap

BSL 1.1 has its own ceiling, independent of the Change Date. A version converts
to MIT on its Change Date **or on the fourth anniversary of the first publicly
available distribution of that version, whichever comes first**. Here the
Change Date is two years, so the two-year date always arrives first and the cap
never binds. It is in the licence as a floor on the Licensor's freedom: no BSL
licensor can hold a published version closed for more than four years.

## Buying a commercial licence

Email **licensing@elcanotek.com**. Say what you want to do with Pages —
internal deployment, hosting it for clients, embedding it in a product — and
roughly at what scale. Commercial terms grant production use and are negotiated
separately from this repository; nothing on this page is an offer.

## Why BSL and not Apache or AGPL

Two goals that a permissive licence cannot hold at the same time: the security
model should be auditable by anyone, and a competitor should not be able to
take the work and run it commercially the day it is published. BSL resolves
that by putting a clock on the restriction instead of making it permanent —
every version becomes MIT eventually, so nothing published here can be
withdrawn from the commons. Fully open source, on a two-year delay.

## Related

- [`LICENSE`](../LICENSE) — the licence itself, and the authority
- [`CONTRIBUTING.md`](../CONTRIBUTING.md) — the terms contributions arrive under
- [`NOTICE`](../NOTICE) — third-party attribution
