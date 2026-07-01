# Fase 1 — Contrato de API e observabilidade

**Duração estimada:** 2–3 semanas · **Risco:** baixo/médio · **Dependências:** Fase 0 (envelope de erro)

Objetivo: transformar o contrato Go↔TypeScript de "espelhado à mão" para gerado a
partir de uma fonte única, e dar ao hub logging estruturado, traces e métricas.
Essas duas frentes destravam a reorganização da Fase 2 (refatorar com telemetria e
contrato formal é muito mais seguro).

---

## 1.1 OpenAPI como fonte única do contrato

**Problema:** ~90 endpoints registrados à mão em `pkg/hub/server.go` (`registerRoutes`),
tipos TS espelhados manualmente em `web/lib/types.ts`. Drift já existente:
`InstanceStatus` no Go inclui `starting` e `deleted`; o union `ClawStatus` no TS não.

**Decisão:** spec-first com `openapi.yaml` na raiz de `api/`, codegen para os dois lados.

- **Go:** `oapi-codegen` gerando `api/gen/` — tipos de request/response e interfaces
  de server (modo `std-http`, compatível com o ServeMux atual). Handlers existentes
  passam a implementar as interfaces geradas gradualmente.
- **TS:** `openapi-typescript` gerando `web/lib/gen/api.d.ts`. `web/lib/types.ts`
  passa a re-exportar/derivar dos tipos gerados; `web/lib/mappers.ts` permanece como
  camada de apresentação (snake_case → camelCase).

**Escopo incremental (não especificar os 90 endpoints de uma vez):**

1. **Lote 1 — endpoints da UI:** `/api/claws*`, `/api/messages*`, `/api/auth/*`,
   `/api/settings*`, `/api/analytics/*`, `/api/templates*`. São os que têm consumidor
   TS e onde o drift dói.
2. **Lote 2 — webhooks de integração** (`/api/integrations/*`): só schemas de payload
   e códigos de resposta (consumidor é externo).
3. **Lote 3 — endpoints internos claw↔hub** (checkpoints, volumes, files): schemas em
   spec separada `api/internal.yaml` (não é API pública).

**Build:** targets `make gen` (roda os dois codegens) e `make gen-check` (gera e falha
se `git diff` não vazio) — `gen-check` entra no CI (`.depot/workflows/main.yaml`).

**Aceite:**
- `make gen-check` verde no CI.
- `web/lib/types.ts` não define mais nenhum tipo espelhado do lote 1 à mão.
- Documentação da API navegável (Redoc/Scalar estático gerado do yaml, servido em
  `/api/docs` apenas em modo dev).

## 1.2 Corrigir o drift de status na UI

**Problema:** UI não trata `starting` nem `deleted`.

**Mudança:** com os tipos gerados do 1.1, o compilador aponta os switches não
exaustivos. Definir apresentação: `starting` renderiza como provisioning (spinner);
`deleted` filtra o claw da lista. Adicionar helper exaustivo
(`assertNever(status)`) para que novos estados quebrem o build em vez de sumirem.

**Aceite:** teste de mapper cobrindo todos os valores do enum gerado.

## 1.3 Logging estruturado + request ID

**Problema:** ~200 `log.Printf` com tags manuais `[component]`; impossível correlacionar
linhas do mesmo request; `fmt.Println` sobrevivendo em `external_storage.go:685,714`.

**Mudança:**

1. Adotar `log/slog` com handler JSON (texto em dev, controlado por
   `ELASTICCLAW_LOG_FORMAT`). Logger raiz criado no boot, injetado no `Server`.
2. Middleware `withRequestID`: gera ID curto, guarda no contexto, adiciona
   `X-Request-ID` na resposta. Helper `logger.FromContext(ctx)` retorna logger com
   `request_id` + `tenant_id` já anexados.
3. Migração mecânica: `log.Printf("[claw] ...")` → `slog` com `component=claw` como
   atributo. Fazer por arquivo, sem mudar mensagens (para não quebrar quem parseia).
4. Proibir `fmt.Println`/`log.Printf` novos via lint (`golangci-lint`: `forbidigo`).

**Aceite:** logs de um request de criação de claw saem todos com o mesmo `request_id`;
`golangci-lint` falha em `log.Printf` novo em `pkg/hub`.

## 1.4 Migrations versionadas

**Problema:** migrations por `ALTER TABLE` idempotente sem versionamento (Fase 0.5 só
parou de engolir erros).

**Mudança:**

1. Adotar migrations numeradas embutidas (`pkg/hub/store/migrations/0001_init.sql`, …)
   com tabela `schema_migrations`. Preferir implementação própria mínima (~80 LOC,
   `//go:embed`) a adicionar `golang-migrate` inteiro — avaliar no primeiro PR; se a
   própria passar de ~150 LOC, usar `golang-migrate` com driver sqlite.
2. `0001_init.sql` = schema atual consolidado (o `CREATE TABLE IF NOT EXISTS` + todos
   os ALTERs de hoje colapsados). Detecção de instalação existente: se as tabelas já
   existem e `schema_migrations` não, registrar baseline como versão 1 sem executar.
3. Migrations rodam no boot dentro de transação (SQLite permite DDL transacional);
   falha aborta o boot.

**Aceite:** upgrade de um `hub.db` real de versão anterior funciona (teste com fixture
de DB antigo commitada); instalação limpa idem; downgrade não suportado — documentado.

## 1.5 Métricas e traces

**Problema:** OTel já está no go.mod (transitivo via SDK Daytona) mas não é usado;
nenhuma métrica exportada.

**Mudança (mínimo útil, não big-bang):**

1. Endpoint `/metrics` Prometheus (usar `prometheus/client_golang`): contadores de
   requests por rota/status, histograma de latência (via middleware), gauges de claws
   por status, mensagens WS in/out, erros de webhook por integração, tamanho do pool
   SQLite.
2. Traces OTel opcionais (`ELASTICCLAW_OTLP_ENDPOINT`): span por request HTTP
   (middleware `otelhttp`) e spans manuais nas operações de provider
   (`Create`/`Exec`/`Destroy`) — são as operações lentas que importam.
3. Sem OTLP endpoint configurado → no-op provider, custo zero.

**Aceite:** `curl /metrics` mostra as séries; trace de criação de claw visível num
collector local (docker compose de dev ganha um serviço `otel-collector` opcional
comentado).

---

## Ordem sugerida de PRs

1. 1.3 (slog + request ID) — beneficia todos os PRs seguintes.
2. 1.4 (migrations) — pequeno e isolado.
3. 1.1 lote 1 + 1.2 (spec + codegen + fix de status) — o maior; pode ser dividido
   em spec/gen primeiro, adoção nos handlers depois.
4. 1.5 (métricas/traces).
5. 1.1 lotes 2 e 3 — podem escorregar para a Fase 2 sem bloquear nada.
