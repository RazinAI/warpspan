# Build libwarpspan.so, the CUPTI injection library.
#
#   make                          # autodetects CUDA in /usr/local/cuda or /opt/cuda
#   make CUDA_HOME=/path/to/cuda
#   make STATIC_CXX=0             # link libstdc++ dynamically (if static linking fails)

CUDA_HOME ?= $(firstword $(wildcard /usr/local/cuda /opt/cuda))
ifeq ($(CUDA_HOME),)
$(error CUDA toolkit not found; set CUDA_HOME=/path/to/cuda)
endif

# CUPTI lives in extras/CUPTI on most installs, in targets/<arch> on some distros.
CUPTI_INC := $(firstword $(wildcard $(CUDA_HOME)/extras/CUPTI/include/cupti.h $(CUDA_HOME)/targets/x86_64-linux/include/cupti.h $(CUDA_HOME)/include/cupti.h))
CUPTI_LIB := $(firstword $(wildcard $(CUDA_HOME)/extras/CUPTI/lib64/libcupti.so $(CUDA_HOME)/targets/x86_64-linux/lib/libcupti.so $(CUDA_HOME)/lib64/libcupti.so))
ifeq ($(CUPTI_INC),)
$(error cupti.h not found under $(CUDA_HOME))
endif
ifeq ($(CUPTI_LIB),)
$(error libcupti.so not found under $(CUDA_HOME))
endif
CUPTI_INC_DIR := $(dir $(CUPTI_INC))
CUPTI_LIB_DIR := $(dir $(CUPTI_LIB))

STATIC_CXX ?= 1

CXXFLAGS ?= -O2 -g
CXXFLAGS += -std=c++17 -Wall -Wextra -fPIC -fvisibility=hidden -fno-exceptions -fno-rtti \
            -I$(CUPTI_INC_DIR) -I$(CUDA_HOME)/include
# RUNPATH: $ORIGIN first (the inject image ships libcupti next to the .so), then
# the build machine's CUPTI dir so a local build works without LD_LIBRARY_PATH.
LDFLAGS  += -shared -Wl,-z,defs -Wl,--exclude-libs,ALL -Wl,--version-script=exports.map \
            -L$(CUPTI_LIB_DIR) -Wl,-rpath,'$$ORIGIN' -Wl,-rpath,$(CUPTI_LIB_DIR)
LDLIBS   += -lcupti -lpthread
ifeq ($(STATIC_CXX),1)
LDFLAGS  += -static-libstdc++ -static-libgcc
endif

libwarpspan.so: warpspan.cpp protocol.h include/warpspan.h exports.map
	$(CXX) $(CXXFLAGS) $(LDFLAGS) -o $@ warpspan.cpp $(LDLIBS)

# Include paths for clangd (editor only; not committed).
compile_flags.txt:
	printf '%s\n' -xc++ -std=c++17 -I$(CUPTI_INC_DIR) -I$(CUDA_HOME)/include > $@

# NVTX label test (no GPU needed): NVTX routes ranges to the library.
test/nvtx_test: test/nvtx_test.cpp
	$(CXX) -std=c++17 -O1 -g -Wall -Wextra -I$(CUDA_HOME)/include -o $@ $< -ldl -lpthread

test/nvtx_module.so: test/nvtx_module.cpp
	$(CXX) -std=c++17 -O1 -g -Wall -Wextra -fPIC -shared -fvisibility=hidden -I$(CUDA_HOME)/include -o $@ $< -ldl

NVTX_TEST_ENV = NVTX_INJECTION64_PATH=$(CURDIR)/libwarpspan.so NVTX_TEST_MODULE=$(CURDIR)/test/nvtx_module.so WARPSPAN_QUIET=1

test: libwarpspan.so test/nvtx_test test/nvtx_module.so
	$(NVTX_TEST_ENV) ./test/nvtx_test
	$(NVTX_TEST_ENV) WARPSPAN_NVTX_MAX_LABELS=2 ./test/nvtx_test cap

clean:
	rm -f libwarpspan.so test/nvtx_test test/nvtx_module.so

.PHONY: clean compile_flags.txt test
