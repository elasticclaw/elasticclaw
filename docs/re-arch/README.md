# Re-arquitetura ElasticClaw — Specs de implementação

Este diretório contém as specs das quatro fases do plano de melhorias de arquitetura,
derivadas da revisão de arquitetura de 2026-07-01 (backend Go, frontend Next.js e
integração/build analisados em varreduras independentes).

## Contexto

O ElasticClaw é um control plane self-hosted distribuído como **binário único Go**
(CLI Cobra + servidor "hub") com a web UI Next.js embutida via `go:embed`. Essa
decisão macro está correta e **não muda**. O plano ataca os problemas internos:

| Problema | Evidência |
|---|---|
| God-package | `pkg/hub` com 103 arquivos / ~54k LOC; `server.go` com 6.535 linhas |
| Sem graceful shutdown | `http.ListenAndServe` direto; 8+ goroutines de fundo sem cancelamento |
| Contexto não propagado | 84 usos de `context.Background()` em `pkg/hub` |
| Contrato Go↔TS manual | Sem OpenAPI; enum de status já divergiu (TS não conhece `starting`/`deleted`) |
| Segurança | Token em query param, CORS `*`, segredos em plaintext no hub.yaml, JWT caseiro |
| Observabilidade | `log.Printf` sem estrutura, OTel presente mas não usado, sem métricas |
| Frontend | `settings-content.tsx` com 4.891 linhas, zero testes, package "my-project" |

## Não-objetivos (valem para todas as fases)

- **Não** migrar para microserviços, GraphQL ou message broker.
- **Não** trocar SQLite como storage padrão (apenas preparar interface para Postgres opcional).
- **Não** trocar `net/http.ServeMux` por framework pesado.
- **Não** reescrever a UI; apenas reorganizar e testar.
- **Não** quebrar o modelo de distribuição por binário único.

## Fases

| Fase | Spec | Duração | Tema |
|---|---|---|---|
| 0 | [fase-0-estancar-riscos.md](fase-0-estancar-riscos.md) | ~1 semana | Shutdown gracioso, recovery, auth/CORS, higiene |
| 1 | [fase-1-contrato-e-observabilidade.md](fase-1-contrato-e-observabilidade.md) | 2–3 semanas | OpenAPI + codegen, slog/OTel/métricas, migrations |
| 2 | [fase-2-reorganizacao-do-hub.md](fase-2-reorganizacao-do-hub.md) | 3–4 semanas | Quebra do `pkg/hub` em subpacotes, concorrência |
| 3 | [fase-3-frontend-e-endurecimento.md](fase-3-frontend-e-endurecimento.md) | 2–3 semanas | Divisão do settings, testes de UI, segredos, config |

As fases são sequenciais na intenção, mas cada uma é entregável de forma independente.
A Fase 1 (contrato OpenAPI) é pré-requisito parcial da Fase 3 (tipos gerados na UI).

## Regras de execução comuns

1. Cada item de spec vira um PR pequeno e revisável; nada de PR "big bang".
2. `make test` e `make test-factory` verdes são gate de merge em todas as fases.
3. Mudanças de comportamento observável (auth, CORS, shutdown) exigem nota no CHANGELOG
   e, quando quebram compatibilidade, flag de transição documentada na spec.
4. A suíte `factorytest` existente é a rede de proteção principal — qualquer refactor
   que exigir mudá-la deve justificar o porquê no PR.
