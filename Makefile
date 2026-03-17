.PHONY: build test clean install lint

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
BUILD_DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

LDFLAGS := -ldflags "-X github.com/elasticclaw/elasticclaw/cmd.Version=$(VERSION) \
	-X github.com/elasticclaw/elasticclaw/cmd.Commit=$(COMMIT) \
	-X github.com/elasticclaw/elasticclaw/cmd.BuildDate=$(BUILD_DATE)"

build:
	go build $(LDFLAGS) -o elasticclaw .

install:
	go install $(LDFLAGS) .

test:
	go test -v ./...

lint:
	golangci-lint run

clean:
	rm -f elasticclaw
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
