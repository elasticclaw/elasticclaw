# Fase 0 — Estancar riscos operacionais

**Duração estimada:** 1 semana · **Risco:** baixo · **Dependências:** nenhuma

Objetivo: remover os riscos que podem causar perda de dados, vazamento de credencial
ou crash em produção **sem** mudar a arquitetura. Todos os itens são localizados e
independentes entre si — um PR por item.

---

## 0.1 Graceful shutdown

**Problema:** `pkg/hub/server.go` sobe com `http.ListenAndServe` e inicia 8+ goroutines
de fundo (`pollProviderStatus`, `keepAliveDaytonaSandboxes`, `pruneAnalytics`,
`statusWatchdog`, `checkpointScheduler`, PR watcher, cron, integration poller) sem
nenhum mecanismo de cancelamento. Um SIGTERM mata o processo no meio de escritas no
SQLite e derruba conexões WS sem despedida.

**Mudança:**

1. Criar um `context.Context` raiz no boot do servidor, cancelado por
   `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)`.
2. Trocar `http.ListenAndServe` por `http.Server` explícito com timeouts
   (`ReadHeaderTimeout`, `IdleTimeout`) e chamar `srv.Shutdown(drainCtx)` no cancel
   (drain de ~15s, configurável).
3. Cada goroutine de fundo passa a receber o ctx raiz e sair no `ctx.Done()`.
   Usar `golang.org/x/sync/errgroup` (já é dependência) para aguardar todas.
4. Ordem de desligamento: parar de aceitar conexões → fechar WS de claws com close
   frame (`"killed"` não é enviado — o sandbox continua vivo) → aguardar goroutines →
   fechar `sql.DB`.
5. `/healthz` retorna 503 durante o drain (hoje retorna 200 sempre).

**Aceite:**
- `kill -TERM` num hub com claw conectado: processo termina em <20s, log mostra a
  sequência de drain, SQLite sem `-wal` órfão sujo.
- Teste de integração: sobe servidor via `factorytest`, envia sinal (ou chama
  `Shutdown()`), verifica que goroutines terminaram (sem leak via `goleak` opcional).

## 0.2 Recovery middleware + envelope de erro

**Problema:** nenhum handler tem recover — um panic derruba a goroutine e, em cadeias
com estado compartilhado, pode corromper invariantes. Respostas de erro alternam entre
`http.Error` texto puro e JSON ad-hoc.

**Mudança:**

1. Middleware `withRecovery` no topo da cadeia (antes de CORS): recover, loga stack
   com request ID (fase 1 adiciona o ID; aqui pode ser o remote addr), responde 500.
2. Definir envelope único de erro e helper:
   ```go
   type apiError struct {
       Error string `json:"error"`
       Code  string `json:"code,omitempty"`
   }
   func writeErr(w http.ResponseWriter, status int, code, msg string)
   ```
3. Migrar apenas os handlers usados pela UI (`/api/claws*`, `/api/messages*`,
   `/api/settings*`, `/api/auth/*`) nesta fase; o restante migra na Fase 2 junto com
   a reorganização. O frontend (`web/lib/api.ts:78-106`) já tenta parsear JSON de erro
   e cai para texto — compatível com migração gradual.

**Aceite:** teste que injeta panic num handler fake e verifica 500 + log; handlers da
UI retornam envelope JSON em erro.

## 0.3 Token fora da query string

**Problema:** `withAuth` (`server.go:~416`) aceita `?token=` como fallback. Tokens
aparecem em access logs, logs de proxy reverso e no histórico de URLs.

**Mudança:**

1. REST: aceitar somente `Authorization: Bearer`. Remover o fallback de query.
2. WebSocket (`/api/ws`, `/api/terminal/{id}`) e recursos usados como `src` de `<img>`
   (`/api/files/view/...`) não podem mandar header pelo browser. Substituir por
   **ticket de uso único**: `POST /api/auth/ticket` (autenticado por header) retorna
   token opaco com TTL de 30s, resgatável uma vez; WS/img usam `?ticket=`.
3. Frontend: `web/hooks/use-hub.ts` e `web/lib/api.ts` passam a pedir ticket antes de
   abrir WS/exibir arquivo. A lógica de redação de URL nos logs
   (`use-hub.ts:42-51`) permanece.
4. Transição: manter `?token=` aceito por 1 release com warning no log
   (`deprecated: token in query`), removível na Fase 2.

**Aceite:** grep no código servidor por `Query().Get("token")` só encontra o caminho
de ticket; teste E2E de terminal e de preview de arquivo passando com ticket.

## 0.4 CORS restrito

**Problema:** `corsMiddleware` responde `Access-Control-Allow-Origin: *`.

**Mudança:** origem permitida vem da config (`hub.yaml: allowed_origins`, default =
origem do próprio hub; em dev, `http://localhost:3000` via `docker/hub.dev.yaml`).
Responder a origem do request se estiver na lista; senão, omitir o header.
`Vary: Origin` sempre.

**Aceite:** compose de dev continua funcionando (web:3000 → hub:8080); request de
origem não listada não recebe header CORS.

## 0.5 Migrations: parar de engolir erros

**Problema:** `pkg/hub/db.go:23-87` executa ~30 `ALTER TABLE` com `_, _ = db.Exec(...)`.
Falha real de migration (disco cheio, lock) fica invisível.

**Mudança (mínima, sem trocar o mecanismo ainda — isso é Fase 1.4):**
classificar o erro: se for "duplicate column name", ignorar; qualquer outro erro,
logar e **abortar o boot**. Helper `execIgnoreDuplicate(db, stmt string) error`.

**Aceite:** teste com DB somente-leitura falha o boot com mensagem clara; boot normal
continua idempotente.

## 0.6 Higiene do frontend

1. `web/package.json`: `"name": "my-project"` → `"elasticclaw-web"`.
2. Remover componentes shadcn gerados e não usados (confirmados: `input-otp`,
   `embla-carousel`/`ui/carousel.tsx`; verificar imports antes de remover cada um).
3. Remover dependências correspondentes do `package.json` e rodar `npm run build`
   para confirmar.

**Aceite:** `npm run build` verde; `npx depcheck` (ou grep de imports) sem deps órfãs
óbvias.

---

## Fora de escopo desta fase

- Request ID / logging estruturado (Fase 1).
- Divisão do `pkg/hub` (Fase 2).
- Criptografia de segredos e JWT padrão (Fase 3).
