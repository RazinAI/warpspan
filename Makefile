# Top-level convenience targets.
#   make            build libwarpspan.so and the agent
#   make test       run Go and Python tests (Python needs opentelemetry-sdk + pytest)

PYTHON ?= python3

all: capture agent

capture:
	$(MAKE) -C capture

agent:
	go build -o bin/warpspan ./cmd/warpspan
	go build -o bin/profiles-dump ./tools/profiles-dump

# opentelemetry-cpp sources for the C++ bridge test (header-only API).
OTEL_CPP ?= $(HOME)/.cache/warpspan-deps/opentelemetry-cpp

test: capture
	go vet ./...
	go test -race ./...
	cd bridge/python && $(PYTHON) -m pytest -q
	cd bridge/go && go vet ./... && go test -race ./...
	@if [ -d "$(OTEL_CPP)/api/include" ]; then $(MAKE) -C bridge/cpp test OTEL_CPP=$(OTEL_CPP); \
	else echo "skipping C++ bridge test: OTEL_CPP=$(OTEL_CPP) not found"; fi

clean:
	$(MAKE) -C capture clean
	$(MAKE) -C bridge/cpp clean
	rm -rf bin

.PHONY: all capture agent test clean
