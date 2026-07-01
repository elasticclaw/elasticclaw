# Fase 3 — Frontend e endurecimento

**Duração estimada:** 2–3 semanas · **Risco:** baixo/médio · **Dependências:**
Fase 1.1 (tipos gerados) para o item 3.2; demais itens independentes

Objetivo: pagar o débito do frontend (god component, zero testes), fechar os itens
de segurança restantes (segredos at rest, JWT) e consolidar a configuração do hub.

---

## 3.1 Dividir o painel de settings

**Problema:** `web/app/settings/[[...parts]]/settings-content.tsx` tem 4.891 linhas
misturando estado, chamadas de API e render de todas as seções do admin; o bundle
de /settings carrega tudo independentemente da aba.

**Mudança:**

1. A rota já é catch-all (`[[...parts]]`) — usar os segmentos para code-splitting:
   um componente por seção em `web/app/settings/sections/`:
   `workspaces.tsx`, `workflows.tsx`, `github.tsx`, `llm-keys.tsx`, `mcp.tsx`,
   `secrets.tsx`, `analytics.tsx`, `ai-config.tsx` (ajustar à divisão real das abas).
2. `settings-content.tsx` vira um shell (~200 LOC): navegação + `React.lazy`/`dynamic`
   por seção.
3. Estado e chamadas de API de cada seção descem para hooks colocalizados
   (`useWorkflowSettings`, `useLLMKeys`, …) — sem contexto global novo.
4. Meta: nenhum arquivo novo >500 LOC; o shell + seções somados podem manter as
   ~4,9k linhas (é divisão, não reescrita), mas cada unidade é testável.

**Aceite:** `npm run build` mostra chunks separados por seção; diff de comportamento
zero (validação manual guiada por checklist das abas).

## 3.2 Tipos gerados na UI + limpeza do use-hub

1. Migrar `web/lib/types.ts` para derivar de `web/lib/gen/api.d.ts` (Fase 1.1);
   apagar as interfaces `Api*` manuais.
2. Extrair de `web/hooks/use-hub.ts` (636 LOC):
   - `useWebSocket` — conexão, backoff exponencial, redação de URL em log
     (reutilizável pelo terminal, que hoje duplica lógica de conexão);
   - `useMessageCache` — persistência localStorage + merge durável/transiente;
   - `useHub` permanece como composição (~250 LOC alvo).
3. Constantes mágicas (chaves de localStorage, intervalos de polling 10s/60s,
   limite de 200 mensagens) → `web/lib/constants.ts`.

**Aceite:** comportamento idêntico (mesmos eventos WS, mesmo cache); terminal usa o
`useWebSocket` extraído.

## 3.3 Testes de frontend

**Problema:** zero testes; refactors 3.1/3.2 sem rede.
**Nota de ordem:** escrever os testes de funções puras **antes** de 3.1/3.2 — eles
são a rede de proteção do refactor.

1. **Vitest** (unit): `lib/mappers.ts`, `lib/attachments.ts`, `lib/auth-storage.ts`
   (funções puras primeiro), depois `useMessageCache`/merge de mensagens com
   `@testing-library/react` (a lógica de merge otimista em `use-hub.ts:276-303` é o
   caso de teste mais valioso do frontend).
2. **Playwright smoke** (1 spec): login por token → lista de claws renderiza →
   abrir conversa → abrir settings. Roda contra o hub de dev do compose no CI
   (job novo em `.depot/workflows/main.yaml`, permitido falhar por 2 semanas até
   estabilizar).
3. Scripts: `npm test`, `npm run test:e2e`; gate de CI para o unit.

**Aceite:** CI roda vitest; cobertura mínima não é meta — a meta é a lógica de merge
de mensagens e mappers cobertas.

## 3.4 Segredos at rest

**Problema:** `hub.yaml` guarda GitHub App private key, tokens Linear/Shortcut/Jira e
senhas em plaintext.

**Mudança:**

1. Envelope de criptografia: valores sensíveis gravados como
   `enc:v1:<base64(nonce+ciphertext)>` usando AES-256-GCM. Chave mestra via
   `ELASTICCLAW_MASTER_KEY` (32 bytes base64) ou arquivo `~/.elasticclaw/master.key`
   criado no primeiro boot (0600).
2. Migração transparente: no load, valor sem prefixo `enc:` é aceito e re-gravado
   cifrado no próximo save. Comando `elasticclaw hub encrypt-secrets` força a
   migração completa.
3. Instalador (`pkg/install/scripts.go`) gera a master key e a injeta no unit
   systemd (`Environment=` ou `EnvironmentFile` 0600).
4. Escopo: campos marcados como sensíveis nos structs de config
   (tag `secret:"true"`), não o arquivo inteiro — o hub.yaml continua legível/editável.

**Aceite:** hub.yaml de instalação nova não contém nenhum segredo em claro;
upgrade de instalação existente migra sem intervenção; perda da master key tem
runbook documentado (re-inserir segredos).

## 3.5 Sessões com golang-jwt

**Problema:** `auth_github.go:32-77` implementa um formato assinado caseiro
(HMAC-SHA256 manual) sendo que `golang-jwt/jwt/v5` já está no go.mod.

**Mudança:** emitir JWT padrão (HS256, claims `sub`, `exp` 7d, `iat`, custom
`login`/`avatar`), chave derivada da master key do 3.4 via HKDF (não mais o
`hubCfg.Token` cru). Aceitar o formato antigo por 1 release (verificação dupla),
depois remover.

**Aceite:** sessões existentes não deslogam no upgrade (janela de transição);
testes de emissão/validação/expiração.

## 3.6 Consolidação de config

**Problema:** dois sistemas — Viper para o CLI, YAML manual para o hub — e o hub.yaml
não suporta hot reload.

**Mudança:**

1. Definir o dono: config do **hub** sai do Viper por completo; loader único em
   `pkg/config/hub.go` com structs tipados + defaults + validação no boot
   (Viper permanece só no CLI, onde faz sentido para flags/env).
2. Hot reload por SIGHUP (padrão de servidor) para o subconjunto seguro:
   `allowed_origins`, branding, log level, integrações. Campos que exigem restart
   (porta, DB path) documentados e rejeitados no reload com log claro.
3. Precedência documentada: flag > env (`ELASTICCLAW_*`) > hub.yaml > default.

**Aceite:** `kill -HUP` aplica mudança de log level sem restart; matriz de
precedência coberta por teste em `pkg/config/hub_test.go`.

## 3.7 Testabilidade do backend (sobra da revisão)

1. `pkg/provider/mock`: implementação in-memory da interface `Provider` (estados
   programáveis, falhas injetáveis) para unit tests sem Daytona/Docker.
2. Unit tests dos handlers extraídos na Fase 2 usando o mock + store em memória.
3. Benchmark do caminho quente: `BenchmarkClawWSMessage` (decode → persist →
   broadcast) como baseline antes de otimizações futuras.

**Aceite:** `go test ./pkg/provider/mock/...` e ao menos 5 handlers principais com
unit test direto (create claw, send message, list claws, settings get/put, webhook
Linear).

---

## Ordem sugerida de PRs

1. 3.3 vitest sobre funções puras (rede de proteção primeiro — antes de 3.1/3.2).
2. 3.2 extração de hooks → 3.1 divisão do settings (nessa ordem: hooks primeiro
   encolhem o god component naturalmente).
3. 3.4 segredos → 3.5 JWT (3.5 depende da master key de 3.4).
4. 3.6 config, 3.7 mock/unit tests, 3.3 Playwright — paralelizáveis.
