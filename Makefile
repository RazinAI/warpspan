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

# opentelemetry-cpp sources for the C++ bridge test (header-only API), and
# the OpenTelemetry jars for the Java bridge test.
OTEL_CPP   ?= $(HOME)/.cache/warpspan-deps/opentelemetry-cpp
MAVEN_JARS ?= $(HOME)/.cache/warpspan-deps/maven
OTEL_JAVA_VERSION ?= 1.68.0

test: capture
	go vet ./...
	go test -race ./...
	cd bridge/python && $(PYTHON) -m pytest -q
	cd bridge/go && go vet ./... && go test -race ./...
	@if [ -d "$(OTEL_CPP)/api/include" ]; then $(MAKE) -C bridge/cpp test OTEL_CPP=$(OTEL_CPP); \
	else echo "skipping C++ bridge test: OTEL_CPP=$(OTEL_CPP) not found"; fi
	@if [ -f "$(MAVEN_JARS)/opentelemetry-api-$(OTEL_JAVA_VERSION).jar" ]; then \
	$(MAKE) -C bridge/java test MAVEN_JARS="$(MAVEN_JARS)" OTEL_VERSION=$(OTEL_JAVA_VERSION); \
	else echo "skipping Java bridge test: no opentelemetry-api-$(OTEL_JAVA_VERSION).jar in MAVEN_JARS=$(MAVEN_JARS)"; fi

clean:
	$(MAKE) -C capture clean
	$(MAKE) -C bridge/cpp clean
	$(MAKE) -C bridge/java clean
	rm -rf bin

.PHONY: all capture agent test clean
