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

test: capture
	go vet ./agent/... ./cmd/warpspan ./tools/...
	go test ./agent/...
	cd bridge/python && $(PYTHON) -m pytest -q

clean:
	$(MAKE) -C capture clean
	rm -rf bin

.PHONY: all capture agent test clean
