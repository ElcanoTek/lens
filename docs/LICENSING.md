# Licensing Lens in plain English

Lens is **source-available**, not open source. The full legal text is in
[`LICENSE`](../LICENSE); this page explains what it means in practice. Where
the two disagree, `LICENSE` wins.

## The four parameters

The Business Source License 1.1 is a template with four blanks. Ours read:

| Parameter | Value |
|---|---|
| Licensor | ElcanoTek, Inc. |
| Additional Use Grant | **None** |
| Change License | MIT |
| Change Date | Two years after the version was first published |

The Additional Use Grant is where a BSL project would carve out limited
production use. Ours says `None`, so there is no production carve-out at all.

## What you may do today, at no cost

- Read the whole codebase.
- Run it for evaluation, development, testing, benchmarking, research,
  teaching or personal experimentation.
- Modify it, fork it, and build derivative works.
- Redistribute it — original or modified — as long as this licence travels
  with it and stays displayed.
- Open issues and pull requests.

## What you may not do without a commercial licence

- **Use it in production.** Anything you or your organisation depends on
  operationally counts: classifying inventory you actually buy or sell against,
  serving results to customers or colleagues who rely on them, or running it
  as part of a paid product or service — internal or external.
- Offer Lens, or a service built on it, to third parties.
- Remove or obscure the licence, or strip the SPDX headers.
- Use ElcanoTek's trademarks or logos beyond what the licence requires.

## What "non-production" actually means

There is no line in the licence that draws this precisely, so use the honest
test: **is anyone relying on the output?**

Probably fine:

- Running a hundred domains through it to see whether the tiers match your own
  judgement.
- Standing it up in a sandbox to read the code and watch the crawl ladder work.
- A student or researcher studying LLM classification pipelines.
- Working on a patch you intend to contribute.

Needs a licence:

- A nightly job feeding your inventory quality dashboard.
- Anything whose results reach a buying, pricing or brand-safety decision.
- A hosted or resold offering built on Lens.
- "It's only internal" — internal operational use is still production.

If you are unsure, ask: **licensing@elcanotek.com**. Asking is free and much
cheaper than guessing wrong.

## The rolling Change Date

This is the part people get wrong, so concretely:

**Every version of Lens converts to the MIT License two years after that
version was published.** The licence applies separately to each version, and
each version carries its own clock.

For the copy in this repository, the Change Date is **two years after the
author date of the commit you are holding**. That is what the helper prints:

```console
$ ./scripts/bsl-change-date.sh
commit:        4d48ab4 (2026-08-12)
Change Date:   2028-08-12
Change License: MIT

$ ./scripts/bsl-change-date.sh v1.2.0     # any git ref works
```

Two consequences:

1. **A new commit does not extend the licence on anything already published.**
   The copy you downloaded in 2026 keeps its own 2028 date and becomes MIT on
   schedule regardless of what we commit afterwards. We cannot claw that back.
2. **The newest code always carries the newest clock.** Each commit starts a
   fresh two years for the version it produces. Track `main` and you are
   always roughly two years from conversion; pin an old commit and its clock
   is already running down.

Once a version's Change Date arrives, that version is MIT — permanent,
irrevocable, and free for production use. What you may not do is take a
converted old version's MIT grant and apply it to newer code that has not
converted yet.

## BSL's own four-year cap

Section 2 of the licence adds a ceiling we cannot exceed: rights under the
Change License take effect on the Change Date **or the fourth anniversary of
the version's first public distribution, whichever comes first**.

So even if a Change Date were somehow set further out, no version of Lens can
stay non-open for more than four years. Our two-year date is well inside that
cap; the cap is a floor on your certainty, not something we rely on.

## What happens if you use it wrong

If your use doesn't comply, you must either buy a commercial licence or stop
using Lens. Continued non-compliant use terminates your rights automatically —
and not only for the version you misused, but for **all** versions.

The practical reading: if you find yourself in production without a licence,
talk to us. That is a conversation, not a lawsuit.

## Buying a commercial licence

Email **licensing@elcanotek.com**. Useful to include:

- What you want to do with Lens.
- Rough scale — items classified per month.
- Whether it stays internal or ships to your customers.
- Whether you need support, or just the production grant.

## Contributing

Contributions are accepted under BSL 1.1 with ElcanoTek as Licensor — the same
terms as the rest of the file you are editing. Opening a pull request means you
have the right to contribute the code and agree to it being released this way.
New source files get:

```python
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
```

See [`CONTRIBUTING.md`](../CONTRIBUTING.md).

## Third-party components

Lens orchestrates Firecrawl (AGPL-3.0) as a separate service over HTTP, ships
reference data from IAB Tech Lab and IANA, and bundles Nebula Sans
(SIL OFL 1.1) as its interface typeface and Hack (MIT) as its monospace face.
None of that is covered by this licence — see [`NOTICE`](../NOTICE).

The bundled IAB Tech Lab Content Taxonomy 3.1 is licensed under CC BY 3.0,
which is permissive and **not copyleft** — it imposes no share-alike
obligation. Bundling it does not change the licence of anything else here: the
taxonomy stays CC BY 3.0 and the rest of the repository stays BSL 1.1. What CC
BY does require is attribution, which `NOTICE` carries, and an indication of
modification if the file is ever edited.

## Frequently asked

**Can I run it on my own inventory internally?**
Not without a licence. Internal operational use is production.

**Can I fork it and relicense my fork?**
No. Derivative works stay under BSL 1.1 until the relevant version converts.

**Can I use the converted MIT version in production?**
Yes, unconditionally, once its Change Date has passed.

**Does a new release reset the clock on my existing copy?**
No. Each published version keeps its own Change Date.

**Is BSL an OSI-approved open-source licence?**
No. Lens is source-available. It becomes open source (MIT) version by version
as each Change Date arrives.
