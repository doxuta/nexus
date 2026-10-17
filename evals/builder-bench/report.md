# builder-bench: the AI builder, scored stage by stage

When a builder asks the Studio assistant for an app, how often does the change apply, publish, and work for a person
who is **not** an admin, and how many tokens does that take? Twenty cases, one score per stage, code graders only.

<!-- RESULTS -->

## What one attempt does

Every attempt runs on the one-command demo stack (`docker compose -p nexus-bench`, port 8093) and starts from the same
seeded state: the `northwind` workspace with the CRM pack, its first version published, the builder
`admin@example.com` and the sales rep `test@example.com` (role `crm_sales_rep`, not an admin).

1. **Reset.** The api container is removed, the seeded database dump is restored, the restored database is dumped again
   and its sha256 must equal the seeded state's (`8c2bed0cbaa4…` for every attempt below), and a new api container
   starts. Nothing survives from the previous attempt: not a row, not a cache, not a rate-limit window.
2. **Ask.** The builder opens the Studio's Data page, types the case's prompt into the assistant panel and presses
   Enter, as a person does. The proposals are read from the conversation the server stored.
3. **Apply.** The builder clicks **Áp dụng tất cả**, then each card left open with its own button, answering the card's
   own question ("Áp ngay cho người dùng thật…"). Nothing is applied by any other path: no API call, no
   re-implementation of `lib/aiApply.ts`. A card that fails to apply makes the panel report the failure to the model
   by itself; the driver waits for that turn to end too, and its tokens count.
4. **Publish** from the header: Xuất bản, the review, Xuất bản.
5. **The rep.** `scripts/acceptance-acl.py --expect <tables>` runs as `test@example.com`.
6. **Smoke.** The rep signs in to the browser and opens each of those tables: from the app switcher and the app's
   menu when the case builds an app, at its URL otherwise.

Adversarial cases stop after step 2: they must end with a reply and no draft.

## The five stages

| stage | passes when |
|---|---|
| kinds | the draft holds every expected kind and no forbidden one (`delete_entity`, `delete_field` for every additive request); an adversarial case holds no draft, and a turn that failed is not a refusal |
| apply | every card of the turn ends **Đã áp dụng**; "Áp dụng một phần" (partly applied), an error, or a card that never rendered fails |
| publish | a new version goes live from the header (a turn that left nothing to publish fails) |
| rep | `acceptance-acl.py` exits 0 **and** each table's row in its own table is ✓ for `test@example.com` itself: a table the gate could only read through an account of another role does not count |
| smoke | the rep opened every table; for an app, from the app switcher |

A case names the tables the rep must use: existing ones (`crm_accounts`) or `@new`, every table the turn proposed.
The edits target the two CRM tables the rep reads in the seeded state (`crm_accounts`, `crm_products`); on the other
CRM tables the seeded rows belong to the installer and the rep's `view: owner` grants show an empty screen, which
`acceptance-acl.py` already fails before any AI change.

The graders live in `nexus-platform/cmd/aieval/bench.go`. The kinds rule is aieval's own (`gradeKinds`, now shared by
the shape run and the bench). `bench_test.go` pins them in CI on a recorded attempt and on synthetic failures, with no
model and no key; each rule was broken once on purpose and the suite went red.

## The cases

<!-- CASES -->
