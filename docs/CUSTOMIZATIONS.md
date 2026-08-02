# ResearchFlow Customizations

This ledger records every ResearchFlow-specific divergence from Tencent/WeKnora upstream on the `researchflow-ext` branch.

## Upstream baseline

- Synchronized on: 2026-08-02
- Upstream remote: `https://github.com/Tencent/WeKnora.git`
- Baseline commit: `5780affdfc76342ddd0f5cf95b548a1a4d0b2a5a`
- Baseline describe: `v0.7.1-115-g5780affd`

## Branch discipline

- `main` only mirrors `upstream/main`; no ResearchFlow-specific commit belongs on `main`.
- Synchronize `main` with `git fetch upstream` followed by `git merge --ff-only upstream/main`.
- Before each W-track substage, synchronize `main`, then merge the synchronized `main` into `researchflow-ext`.
- Keep extensions on connector, parser, provider, or DTO plugin surfaces whenever possible.
- Before changing a core pipeline, add the proposed divergence to this ledger and record in a ResearchFlow ADR why the plugin surface is insufficient.
- Evaluate every customization for contribution to upstream and record expected resynchronization impact.

## Entry requirements

Each entry must record its W-track stage, scope, reason, changed files, tests, upstream contribution suitability, and expected impact when resynchronizing with upstream.

## Customization entries

| ID | Stage | Scope | Reason | Files | Tests | Upstream contribution | Resynchronization impact |
|---|---|---|---|---|---|---|---|
