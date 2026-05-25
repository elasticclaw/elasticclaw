.PHONY: build build-bridge build-bridge-linux test test-bootstrap test-container e2e clean install lint tidy clawpatch-init clawpatch-review clawpatch-report clawpatch-show clawpatch-triage clawpatch-pr


VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
BUILD_DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

LDFLAGS := -ldflags "-X github.com/elasticclaw/elasticclaw/cmd.Version=$(VERSION) \
	-X github.com/elasticclaw/elasticclaw/cmd.Commit=$(COMMIT) \
	-X github.com/elasticclaw/elasticclaw/cmd.BuildDate=$(BUILD_DATE)"

tidy:
	go mod tidy

# Fast build — Go only, no npm. Uses existing internal/webui/out/ (or placeholder).
build: build-dev
build-dev:
	mkdir -p bin
	go build $(LDFLAGS) -o bin/elasticclaw .

# Build the Next.js static export and copy into internal/webui/out/ for embedding.
build-web:
	@command -v npm >/dev/null 2>&1 || (echo "❌ npm not found in PATH — install Node.js" && exit 1)
	cd web && npm install && npm run build
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
	go test -v -tags integration -timeout 120s ./pkg/hub/... -run TestParity

e2e: build-dev ## Run the real Daytona + GitHub Issues E2E suite
	@test -n "$$ELASTICCLAW_E2E_PUBLIC_URL" || (echo "ELASTICCLAW_E2E_PUBLIC_URL is required (start ngrok and export its https URL)" && exit 1)
	ELASTICCLAW_E2E=1 \
	ELASTICCLAW_E2E_BIN="$(CURDIR)/bin/elasticclaw" \
	ELASTICCLAW_E2E_HUB_ADDR="$${ELASTICCLAW_E2E_HUB_ADDR:-127.0.0.1:8080}" \
	go test -v ./test/e2e -run TestDaytonaGitHubIssuesWorkflowE2E -count=1 -timeout 30m

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
