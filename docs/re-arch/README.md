# ElasticClaw Re-architecture — Implementation Specs

This directory contains the specs for the four phases of the architecture
improvement plan, derived from the 2026-07-01 architecture review (Go backend,
Next.js frontend, and integration/build analyzed in independent sweeps).

## Context

ElasticClaw is a self-hosted control plane shipped as a **single Go binary**
(Cobra CLI + "hub" server) with the Next.js web UI embedded via `go:embed`. That
macro decision is right and **does not change**. This plan targets the internal
problems:

| Problem | Evidence |
|---|---|
| God package | `pkg/hub` with 103 files / ~54k LOC; `server.go` at 6,535 lines |
| No graceful shutdown | Bare `http.ListenAndServe`; 8+ background goroutines with no cancellation |
| Context not propagated | 84 uses of `context.Background()` in `pkg/hub` |
| Hand-maintained Go↔TS contract | No OpenAPI; the status enum has already drifted (TS doesn't know `starting`/`deleted`) |
| Security | Token in query params, CORS `*`, plaintext secrets in hub.yaml, hand-rolled JWT |
| Observability | Unstructured `log.Printf`, OTel present but unused, no metrics |
| Frontend | `settings-content.tsx` at 4,891 lines, zero tests, package named "my-project" |

## Non-goals (apply to every phase)

- Do **not** migrate to microservices, GraphQL, or a message broker.
- Do **not** replace SQLite as the default storage (only prepare the interface for optional Postgres).
- Do **not** replace `net/http.ServeMux` with a heavyweight framework.
- Do **not** rewrite the UI; only reorganize and test it.
- Do **not** break the single-binary distribution model.

## Phases

| Phase | Spec | Duration | Theme |
|---|---|---|---|
| 0 | [phase-0-stop-the-bleeding.md](phase-0-stop-the-bleeding.md) | ~1 week | Graceful shutdown, recovery, auth/CORS, hygiene |
| 1 | [phase-1-contract-and-observability.md](phase-1-contract-and-observability.md) | 2–3 weeks | OpenAPI + codegen, slog/OTel/metrics, migrations |
| 2 | [phase-2-hub-reorganization.md](phase-2-hub-reorganization.md) | 3–4 weeks | Splitting `pkg/hub` into subpackages, concurrency |
| 3 | [phase-3-frontend-and-hardening.md](phase-3-frontend-and-hardening.md) | 2–3 weeks | Settings split, UI tests, secrets, config |

The phases are sequential in intent, but each one is independently shippable.
Phase 1 (OpenAPI contract) is a partial prerequisite of Phase 3 (generated types
in the UI).

## Common execution rules

1. Each spec item becomes a small, reviewable PR; no big-bang PRs.
2. Green `make test` and `make test-factory` are the merge gate in every phase.
3. Changes to observable behavior (auth, CORS, shutdown) require a CHANGELOG note
   and, when they break compatibility, a transition flag documented in the spec.
4. The existing `factorytest` suite is the primary safety net — any refactor that
   requires changing it must justify why in the PR.
