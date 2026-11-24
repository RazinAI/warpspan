# Builds and runs the C++ bridge test. Needs the opentelemetry-cpp sources
# (only the header-only API is used):
#   git clone --depth 1 --branch v1.30.0 https://github.com/open-telemetry/opentelemetry-cpp
#   make test OTEL_CPP=path/to/opentelemetry-cpp

OTEL_CPP ?= $(HOME)/.cache/warpspan-deps/opentelemetry-cpp
LIB      := ../../capture/libwarpspan.so

CXXFLAGS ?= -O1 -g
override CXXFLAGS += -std=c++17 -Wall -Wextra -Iinclude -I../../capture/include -I$(OTEL_CPP)/api/include

test: bridge_test capture
	./bridge_test $(LIB)

bridge_test: test/bridge_test.cpp include/warpspan/otel_bridge.h ../../capture/include/warpspan.h
	$(CXX) $(CXXFLAGS) -o $@ test/bridge_test.cpp -ldl -pthread

capture:
	$(MAKE) -C ../../capture

clean:
	rm -f bridge_test

.PHONY: test capture clean
