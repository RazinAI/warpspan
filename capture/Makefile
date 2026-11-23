CXXFLAGS ?= -O2 -g -Wall -Wextra
CXXFLAGS += -std=c++20 -fPIC -fvisibility=hidden
LDFLAGS  += -shared

libwarpspan.so: warpspan.cpp
	$(CXX) $(CXXFLAGS) $(LDFLAGS) -o $@ $<

clean:
	rm -f libwarpspan.so

.PHONY: clean
