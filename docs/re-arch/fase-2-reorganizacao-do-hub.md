# Fase 2 — Reorganização do pkg/hub

**Duração estimada:** 3–4 semanas · **Risco:** médio · **Dependências:** Fases 0 e 1
(shutdown, request ID e testes de contrato tornam o refactor observável e seguro)

Objetivo: quebrar o god-package `pkg/hub` (103 arquivos, ~54k LOC, `server.go` com
6.535 linhas) em subpacotes por domínio, com fronteiras de concorrência e dados
explícitas. **Migração mecânica de código existente — não é reescrita.** A suíte
`factorytest` + testes de integração existentes são o gate de cada movimento.

---

## 2.1 Estrutura alvo

```
pkg/hub/
├── httpserver/      # router, middleware chain, envelope de erro, helpers HTTP
├── claws/           # lifecycle de claw, conexões WS (/claw/ws), files, streaming
├── integrations/    # linear.go, github_*.go, jira.go, shortcut.go, external_webhook.go,
│                    # integration_poller.go — um subdiretório por tracker se crescer
├── workflows/       # pipeline_runner, factory_*, cron_*, pr_watcher
├── checkpoints/     # checkpoints, volumes, external_storage, artifacts
├── analytics/       # task_run_analytics*
├── settings/        # settings, ai_config, model_auth, doctor
├── store/           # repositórios + migrations (ver 2.4)
└── hub.go           # composição: monta store → serviços → httpserver; Run(ctx)
```

Regras de dependência (verificadas por `depguard` no golangci-lint):

- `httpserver` conhece os serviços; serviços **não** conhecem `httpserver`.
- Serviços conhecem `store` e `pkg/types`; `store` só conhece `pkg/types`.
- `integrations`/`workflows` podem chamar `claws` (criar claw a partir de evento);
  `claws` não conhece nenhum dos dois.
- Ciclos proibidos; se aparecer necessidade de ciclo, o tipo compartilhado desce
  para `pkg/types` ou nasce uma interface no consumidor.

## 2.2 Roteiro de extração (ordem de menor acoplamento primeiro)

Cada passo é um PR; `git mv` para preservar histórico; sem mudança de comportamento.

1. **`httpserver/`** — extrair `registerRoutes`, middlewares e helpers de resposta
   de `server.go`. Handlers ainda vivem onde estão; o router referencia por interface.
2. **`analytics/`** — mais isolado (task_run_analytics* ~3,4k LOC + testes próprios).
3. **`settings/`** — settings, ai_config, model_auth, doctor.
4. **`integrations/`** — webhooks + pollers; junto, mover a validação de assinatura
   para helper comum e **auditar que todas as integrações validam assinatura**
   (achado da revisão: não confirmado para todas).
5. **`checkpoints/`** — checkpoints, volumes, external_storage.
6. **`workflows/`** — pipeline_runner, factory_*, cron, pr_watcher.
7. **`claws/`** — por último (é o miolo): handleClawWS, streaming, files, terminal.
8. **`server.go` residual → `hub.go`** — só composição e ciclo de vida.

Meta quantitativa: nenhum arquivo >1.500 LOC ao final; `server.go`/`hub.go` <500 LOC.

## 2.3 Concorrência

**Problemas:** mutex único do `Server` protegendo claws, users, config, dedup de
webhook e cache de modelos; goroutine por mensagem WS sem bound (`server.go:2546`);
84 `context.Background()`.

**Mudanças:**

1. **Um mutex por subsistema**, movido junto com o estado na extração do 2.2:
   `claws.registry` (RWMutex próprio), `users.registry`, `settings.cache`, etc.
   Nenhum lock global sobra em `hub.go`.
2. **Worker pool para mensagens WS**: substituir o `go func(payload, conn)` por
   pool com `semaphore.Weighted` (x/sync já é dep) — limite configurável
   (default 64 concorrentes por hub), backpressure fecha a conexão com close code
   próprio se a fila estourar (proteção contra client malicioso/loop).
3. **Varredura de `context.Background()`**: em handler HTTP → `r.Context()`; em
   trabalho que deve sobreviver ao request (ex.: provisionar sandbox após webhook) →
   derivar do ctx raiz do servidor (`hub.baseCtx`) com timeout explícito, nunca
   `Background()` cru. Lint `contextcheck` ligado para impedir regressão.
4. **Streaming com commits parciais**: hoje `streamingBuf` acumula chunks em memória
   e só persiste no fim — timeout no meio perde tudo. Persistir segmento parcial a
   cada N bytes/segundos (mecanismo de `streamingSplit` já existe; passar a usá-lo
   como flush periódico), marcando o registro como parcial até o fim do stream.

**Aceite:** teste de carga simples (script em `hack/`) com 100 claws simulados não
cresce goroutines de forma não-bounded (`runtime.NumGoroutine` estável); teste de
streaming com desconexão no meio preserva o conteúdo já recebido.

## 2.4 Camada de dados (`store/`)

**Problema:** SQL cru espalhado pelos handlers (`db.Exec`/`QueryRow` direto),
sem transações em operações multi-passo.

**Mudança:**

1. Repositório por agregado: `store.Claws`, `store.Messages`, `store.Checkpoints`,
   `store.Workflows`, `store.Analytics`, `store.Settings`. Interfaces definidas no
   pacote consumidor quando necessário para teste; implementação SQLite única.
2. Operações multi-passo (criar claw + mensagem inicial + trigger de analytics)
   ganham método transacional no repositório (`store.WithTx(ctx, fn)`).
3. Tratamento de `SQLITE_BUSY`: retry curto com backoff no wrapper do store
   (não nos handlers).
4. **Preparação para Postgres opcional (não implementação):** nenhum SQL com
   sintaxe exclusiva SQLite fora de `store/`; placeholders via constante. A decisão
   de suportar Postgres fica para depois — esta fase só não fecha a porta.

**Aceite:** `grep -r "db.Exec\|db.Query" pkg/hub --include='*.go' | grep -v store/`
vazio ao final; testes de repositório com DB em memória.

## 2.5 Erros de domínio e payloads WS tipados

1. Sentinel errors em `pkg/types/errors.go`: `ErrClawNotFound`, `ErrTenantMismatch`,
   `ErrWorkflowNotFound`, `ErrUnauthorized`… O `httpserver` mapeia
   `errors.Is` → status HTTP num único lugar (encerra a inconsistência de respostas).
2. `WSMessage.Payload interface{}` → decodificação tipada: manter o envelope
   `{type, payload: json.RawMessage}` no wire (sem quebra de protocolo) e trocar as
   type assertions por um registry `map[string]func(json.RawMessage) (Handler, error)`
   com unmarshal direto no tipo concreto. Panics de assertion viram erro de protocolo
   logado.

**Aceite:** nenhum type assertion sobre `Payload` fora do decoder; tabela de
mapeamento erro→status coberta por teste.

## 2.6 Remoções de compatibilidade

- Remover o fallback deprecado `?token=` (transição iniciada na Fase 0.3).
- Migrar os handlers restantes para o envelope de erro (iniciado na Fase 0.2).

---

## Riscos e mitigação

| Risco | Mitigação |
|---|---|
| Refactor quebra comportamento sutil de WS/streaming | Extrair `claws/` por último; testes de parity (`make test-parity`) como gate |
| PRs de `git mv` gigantes difíceis de revisar | Um subpacote por PR; commit de move separado do commit de ajuste de imports |
| Conflito com features em desenvolvimento paralelo | Janela de código-congelado combinada por subpacote antes de cada extração |
| Mutexes divididos introduzem deadlock | Ordem de aquisição documentada em `hub.go`; `go test -race` obrigatório no CI (verificar se já está) |
