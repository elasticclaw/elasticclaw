.PHONY: gen gen-check build build-bridge build-bridge-linux test test-bootstrap test-container e2e e2e-github e2e-linear e2e-jira e2e-replicated-github e2e-replicated-linear e2e-replicated-jira e2e-docker e2e-run clean install lint tidy clawpatch-init clawpatch-review clawpatch-report clawpatch-show clawpatch-triage clawpatch-pr dev dev-up dev-up-d dev-down dev-reset dev-logs dev-restart dev-sh-hub dev-sh-web dev-agent-build dev-claw _dev-config-check


VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
BUILD_DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

LDFLAGS := -ldflags "-X github.com/elasticclaw/elasticclaw/cmd.Version=$(VERSION) \
	-X github.com/elasticclaw/elasticclaw/cmd.Commit=$(COMMIT) \
	-X github.com/elasticclaw/elasticclaw/cmd.BuildDate=$(BUILD_DATE)"

tidy:
	go mod tidy

# ─── API contract codegen ─────────────────────────────────────────────────────
# api/openapi.yaml is the single source of the Go<->TypeScript contract.
# `make gen` regenerates both sides; `make gen-check` fails when the generated
# artifacts are stale (run it in CI / before pushing contract changes).

gen: ## Regenerate Go and TypeScript API types from api/openapi.yaml
	go tool oapi-codegen -config api/oapi-codegen.yaml api/openapi.yaml
	cd web && npm run gen

gen-check: gen ## Fail if generated API artifacts are out of date
	@git diff --exit-code -- api/gen web/lib/gen || \
		(echo "❌ generated API artifacts are stale — run 'make gen' and commit the result" && exit 1)

# Fast build — Go only, no npm. Uses existing internal/webui/out/ (or placeholder).
build: build-dev
build-dev:
	mkdir -p bin
	go build $(LDFLAGS) -o bin/elasticclaw .

# Build the Next.js static export and copy into internal/webui/out/ for embedding.
build-web:
	@command -v npm >/dev/null 2>&1 || (echo "❌ npm not found in PATH — install Node.js" && exit 1)
	cd web && npm install && npm run build
	cd web && ./scripts/check-static-export.sh out
	mkdir -p internal/webui/out
	rm -rf internal/webui/out && cp -r web/out internal/webui/out

# Full production build — compiles web UI then embeds in Go binary.
build-release: build-web
	mkdir -p bin
	go build $(LDFLAGS) -tags embedweb -o bin/elasticclaw .


build-bridge:
	mkdir -p bin
	CGO_ENABLED=0 go build -ldflags "-X main.Version=$(VERSION) -X main.Commit=$(COMMIT) -X main.BuildDate=$(BUILD_DATE)" -o bin/claw-bridge ./cmd/claw-bridge/

build-bridge-linux:
	mkdir -p bin
	GONOSUMDB=* CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-X main.Version=$(VERSION) -X main.Commit=$(COMMIT) -X main.BuildDate=$(BUILD_DATE)" -o bin/claw-bridge-linux-amd64 ./cmd/claw-bridge/


install:
	go install -buildvcs=false $(LDFLAGS) .

test:
	go test -v ./...

test-factory: ## Run factory integration tests
	go test -v -tags integration -timeout 60s ./pkg/hub/... -run TestFactory

test-parity: ## Run parity matrix integration tests (all trackers)
	go test -v -tags integration -timeout 300s ./pkg/hub/... -run TestParity

e2e: e2e-github e2e-linear e2e-jira e2e-replicated-github e2e-replicated-linear e2e-replicated-jira e2e-docker ## Run all real E2E suites sequentially

e2e-github: ## Run the real Daytona + GitHub Issues E2E suite
	$(MAKE) e2e-run E2E_TEST=TestDaytonaGitHubIssuesWorkflowE2E

e2e-linear: ## Run the real Daytona + Linear E2E suite
	$(MAKE) e2e-run E2E_TEST=TestDaytonaLinearWorkflowE2E

e2e-jira: ## Run the real Daytona + Jira Cloud E2E suite
	$(MAKE) e2e-run E2E_TEST=TestDaytonaJiraWorkflowE2E

e2e-replicated-github: ## Run the real Replicated CMX + GitHub Issues E2E suite
	$(MAKE) e2e-run E2E_TEST=TestReplicatedGitHubIssuesWorkflowE2E

e2e-replicated-linear: ## Run the real Replicated CMX + Linear E2E suite
	$(MAKE) e2e-run E2E_TEST=TestReplicatedLinearWorkflowE2E

e2e-replicated-jira: ## Run the real Replicated CMX + Jira Cloud E2E suite
	$(MAKE) e2e-run E2E_TEST=TestReplicatedJiraWorkflowE2E

e2e-docker: ## Run the real Docker workflow E2E suite
	$(MAKE) e2e-run E2E_TEST=TestDockerWorkflowE2E

e2e-run: build-dev build-bridge-linux
	@command -v ngrok >/dev/null 2>&1 || (echo "ngrok is required for make e2e" && exit 1)
	@command -v python3 >/dev/null 2>&1 || (echo "python3 is required for make e2e" && exit 1)
	@test -n "$$NGROK_AUTHTOKEN" || (echo "NGROK_AUTHTOKEN is required for make e2e" && exit 1)
	@test -n "$$NGROK_API_KEY" || (echo "NGROK_API_KEY is required for make e2e so temporary reserved domains can be created and deleted" && exit 1)
	@set -e; \
	HUB_ADDR="$${ELASTICCLAW_E2E_HUB_ADDR:-127.0.0.1:8080}"; \
	HUB_PORT="$${HUB_ADDR##*:}"; \
	NGROK_API_ADDR="$${ELASTICCLAW_E2E_NGROK_API_ADDR:-127.0.0.1:4049}"; \
	NGROK_LOG="$$(mktemp -t elasticclaw-ngrok.XXXXXX.log)"; \
	NGROK_CONFIG="$$(mktemp -t elasticclaw-ngrok.XXXXXX.yml)"; \
	NGROK_DOMAIN_JSON="$$(mktemp -t elasticclaw-ngrok-domain.XXXXXX.json)"; \
	DAYTONA_IDS="$$(mktemp -t elasticclaw-daytona-sandbox-ids.XXXXXX)"; \
	REPLICATED_IDS="$$(mktemp -t elasticclaw-replicated-vm-ids.XXXXXX)"; \
	E2E_RUN_ID_RAW="$${ELASTICCLAW_E2E_RUN_ID:-$$(python3 -c 'import time,uuid; print(f"{time.time_ns()}-{uuid.uuid4().hex[:8]}")')}"; \
	E2E_RUN_ID="$$(printf '%s' "$$E2E_RUN_ID_RAW" | python3 -c 'import sys; value=sys.stdin.read().strip().lower(); out="".join(ch if ch.isalnum() and ch.isascii() or ch == "-" else "-" for ch in value).strip("-") or "run"; print((out[:32].strip("-")) or "run")')"; \
	NGROK_PID=""; \
	NGROK_DOMAIN_ID=""; \
	printf 'version: "3"\nagent:\n  web_addr: "%s"\n' "$$NGROK_API_ADDR" > "$$NGROK_CONFIG"; \
	cleanup() { code="$$?"; ELASTICCLAW_E2E_DAYTONA_SANDBOX_ID_FILE="$$DAYTONA_IDS" ELASTICCLAW_E2E_REPLICATED_VM_ID_FILE="$$REPLICATED_IDS" go test -tags e2e -v ./test/e2e -run 'TestCleanupRecorded(DaytonaSandboxes|ReplicatedVMs)' -count=1 -timeout 6m >/dev/null 2>&1 || true; if [ -n "$$NGROK_PID" ]; then kill "$$NGROK_PID" >/dev/null 2>&1 || true; fi; if [ -n "$$NGROK_DOMAIN_ID" ]; then ngrok api reserved-domains delete "$$NGROK_DOMAIN_ID" --api-key "$$NGROK_API_KEY" >/dev/null 2>&1 || true; fi; rm -f "$$NGROK_LOG" "$$NGROK_CONFIG" "$$NGROK_DOMAIN_JSON" "$$DAYTONA_IDS" "$$REPLICATED_IDS"; exit "$$code"; }; \
	trap cleanup EXIT INT TERM; \
	NGROK_HOST="ec-$$(git rev-parse --short HEAD 2>/dev/null || echo dev)-$$E2E_RUN_ID.ngrok-free.app"; \
	echo "Creating temporary ngrok reserved domain https://$$NGROK_HOST"; \
	for attempt in $$(seq 1 5); do \
		if ngrok api reserved-domains create --api-key "$$NGROK_API_KEY" --domain "$$NGROK_HOST" --description "ElasticClaw E2E temporary tunnel" --metadata "elasticclaw-e2e" > "$$NGROK_DOMAIN_JSON"; then \
			break; \
		fi; \
		if [ "$$attempt" -eq 5 ]; then \
			cat "$$NGROK_DOMAIN_JSON" 2>/dev/null || true; \
			echo "ngrok reserved domain create failed after $$attempt attempts"; \
			exit 1; \
		fi; \
		cat "$$NGROK_DOMAIN_JSON" 2>/dev/null || true; \
		sleep_seconds=$$((attempt * 5)); \
		echo "ngrok reserved domain create failed on attempt $$attempt; retrying in $$sleep_seconds seconds"; \
		sleep "$$sleep_seconds"; \
	done; \
	NGROK_DOMAIN_ID="$$(python3 -c 'import json,sys; print(json.load(sys.stdin).get("id", ""))' < "$$NGROK_DOMAIN_JSON")"; \
	if [ -z "$$NGROK_DOMAIN_ID" ]; then cat "$$NGROK_DOMAIN_JSON"; echo "ngrok reserved domain create did not return an id"; exit 1; fi; \
	sleep 5; \
	echo "Starting ngrok tunnel https://$$NGROK_HOST for localhost:$$HUB_PORT"; \
	ngrok http "$$HUB_PORT" --url "https://$$NGROK_HOST" --authtoken "$$NGROK_AUTHTOKEN" --config "$$NGROK_CONFIG" --log stdout > "$$NGROK_LOG" 2>&1 & \
	NGROK_PID="$$!"; \
	echo "Waiting for ngrok tunnel https://$$NGROK_HOST on localhost:$$HUB_PORT..."; \
	for i in $$(seq 1 90); do \
		if ! kill -0 "$$NGROK_PID" >/dev/null 2>&1; then cat "$$NGROK_LOG"; echo "ngrok exited before tunnel was ready"; exit 1; fi; \
		ELASTICCLAW_E2E_PUBLIC_URL="$$(curl -fsS "http://$$NGROK_API_ADDR/api/tunnels" 2>/dev/null | python3 -c 'import json,sys; data=json.load(sys.stdin); want="https://" + sys.argv[1]; print(next((t["public_url"] for t in data.get("tunnels", []) if t.get("proto") == "https" and t.get("public_url") == want), ""))' "$$NGROK_HOST" 2>/dev/null || true)"; \
		if [ -n "$$ELASTICCLAW_E2E_PUBLIC_URL" ]; then \
			case "$$ELASTICCLAW_E2E_PUBLIC_URL" in *://elasticclaw.ngrok.app*) echo "Refusing shared ngrok domain: $$ELASTICCLAW_E2E_PUBLIC_URL"; exit 1;; esac; \
			echo "ngrok: $$ELASTICCLAW_E2E_PUBLIC_URL"; \
			ELASTICCLAW_E2E_BIN="$(CURDIR)/bin/elasticclaw" \
			ELASTICCLAW_E2E_BRIDGE_BINARY="$(CURDIR)/bin/claw-bridge-linux-amd64" \
			ELASTICCLAW_E2E_DAYTONA_SANDBOX_ID_FILE="$$DAYTONA_IDS" \
			ELASTICCLAW_E2E_REPLICATED_VM_ID_FILE="$$REPLICATED_IDS" \
			ELASTICCLAW_E2E_HUB_ADDR="$$HUB_ADDR" \
			ELASTICCLAW_E2E_PUBLIC_URL="$$ELASTICCLAW_E2E_PUBLIC_URL" \
			ELASTICCLAW_E2E_RUN_ID="$$E2E_RUN_ID" \
			go test -tags e2e -v ./test/e2e -run "$${E2E_TEST:-TestDaytonaGitHubIssuesWorkflowE2E}" -count=1 -timeout 30m; \
			exit "$$?"; \
		fi; \
		sleep 1; \
	done; \
	cat "$$NGROK_LOG"; \
	echo "ngrok did not report a public https tunnel"; \
	exit 1

# Run only bootstrap unit tests (fast, no infra needed)
test-bootstrap:
	go test -v ./pkg/hub/ -run TestBootstrap

# Run container integration tests (requires Docker + ELASTICCLAW_TEST_BRIDGE_URL)
test-container:
	ELASTICCLAW_CONTAINER_TESTS=1 go test -v ./pkg/hub/ -run TestBootstrap_ContainerRun -timeout 10m

# Run install integration tests in a real Ubuntu container (requires Docker)
test-install:
	ELASTICCLAW_INSTALL_TESTS=1 go test -tags integration -v ./pkg/install/ -run TestInstall_Container -timeout 5m

lint:
	golangci-lint run

clawpatch-init:
	hack/clawpatch-pr.sh init

clawpatch-review:
	hack/clawpatch-pr.sh review

clawpatch-report:
	hack/clawpatch-pr.sh report

clawpatch-show:
	hack/clawpatch-pr.sh show

clawpatch-triage:
	hack/clawpatch-pr.sh triage

clawpatch-pr:
	hack/clawpatch-pr.sh pr

clean:
	rm -rf bin/
	rm -rf .elasticclaw/

# ── Docker dev environment ────────────────────────────────────────────────────
# Prerequisites: Docker Desktop (or Docker Engine) running.
# make is already installed on macOS via Xcode Command Line Tools.
#
# Quick start:
#   cp docker/hub.dev.yaml.example docker/hub.dev.yaml
#   make dev                                              # builds + starts everything
#   make dev-ollama-pull                                  # pulls the default local test model
#   open http://localhost:3000  (login: devpass)
#   make dev-claw                                         # spawn a local agent

COMPOSE := docker compose -f docker/compose.dev.yml
MODEL ?= qwen2.5-coder:1.5b

# Ensure docker/hub.dev.yaml exists as a FILE before any compose command.
# Docker creates an empty directory when a bind-mounted file is missing, which
# breaks the hub config. This guard creates the file from the example and
# prompts the user to fill in their LLM key.
_dev-config-check:
	@if [ ! -f docker/hub.dev.yaml ]; then \
		cp docker/hub.dev.yaml.example docker/hub.dev.yaml; \
		echo ""; \
		echo "  Created docker/hub.dev.yaml from example."; \
		echo "  It defaults to local Ollama. Run make dev again, then make dev-ollama-pull."; \
		echo "  Edit docker/hub.dev.yaml if you want to use an external LLM API."; \
		echo ""; \
		exit 1; \
	fi

dev: _dev-config-check dev-agent-build dev-up	## Build agent image then start hub + web (foreground)

dev-up: _dev-config-check		## Start hub + web (builds hub image if needed)
	$(COMPOSE) up --build

dev-up-d: _dev-config-check		## Start hub + web in background (detached)
	$(COMPOSE) up --build -d

dev-down:			## Stop containers (preserves DB volume)
	$(COMPOSE) down

dev-reset:			## Stop containers AND delete all volumes (clean slate)
	$(COMPOSE) down -v

dev-logs:			## Follow logs from hub and web
	$(COMPOSE) logs -f

dev-restart:			## Restart just the hub (picks up hub.dev.yaml changes)
	$(COMPOSE) restart hub

dev-sh-hub:			## Open a shell in the running hub container
	$(COMPOSE) exec hub sh

dev-sh-web:			## Open a shell in the running web container
	$(COMPOSE) exec web sh

dev-agent-build:		## Build the local agent container image (elasticclaw/claw-agent:dev)
	docker build -f docker/agent.Dockerfile -t elasticclaw/claw-agent:dev .

dev-ollama-pull:		## Pull the local Ollama model (override with MODEL=model:tag)
	$(COMPOSE) exec ollama ollama pull $(MODEL)

dev-claw:			## Spawn a one-off local agent via the docker provider
	@curl -fsS -X POST http://localhost:8080/api/claws \
	  -H "Authorization: Bearer devtoken" \
	  -H "Content-Type: application/json" \
	  -d '{"name":"local-agent","provider":"docker"}' | cat

# ─────────────────────────────────────────────────────────────────────────────

# Development helpers
dev-template:
	rm -rf /tmp/test-elasticclaw
	mkdir -p /tmp/test-elasticclaw
	cd /tmp/test-elasticclaw && $(CURDIR)/elasticclaw template new --name test-agent
	cd /tmp/test-elasticclaw && $(CURDIR)/elasticclaw init
	@echo "Test template created at /tmp/test-elasticclaw"

dev-create:
	cd /tmp/test-elasticclaw && $(CURDIR)/elasticclaw create --name test-01 --provider local

dev-clean:
	cd /tmp/test-elasticclaw && $(CURDIR)/elasticclaw destroy --all --force || true
	rm -rf /tmp/test-elasticclaw
