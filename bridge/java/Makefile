# Builds the Java bridge jar and runs its test. Needs JDK 22+ and the
# OpenTelemetry jars (api, context, common; sdk-trace and sdk-common for the test):
#   make test MAVEN_JARS=dir/with/jars OTEL_VERSION=1.68.0

MAVEN_JARS   ?= $(HOME)/.cache/warpspan-deps/maven
OTEL_VERSION ?= 1.68.0
LIB          := ../../capture/libwarpspan.so

API_CP  := $(MAVEN_JARS)/opentelemetry-api-$(OTEL_VERSION).jar:$(MAVEN_JARS)/opentelemetry-context-$(OTEL_VERSION).jar:$(MAVEN_JARS)/opentelemetry-common-$(OTEL_VERSION).jar
TEST_CP := $(API_CP):$(MAVEN_JARS)/opentelemetry-sdk-trace-$(OTEL_VERSION).jar:$(MAVEN_JARS)/opentelemetry-sdk-common-$(OTEL_VERSION).jar
JAVA_RUN := java --enable-native-access=ALL-UNNAMED -cp build/classes:build/test-classes:$(TEST_CP)

SRC  := $(shell find src/main/java -name '*.java')
TEST := $(shell find src/test/java -name '*.java')

jar: build/warpspan-otel.jar

build/warpspan-otel.jar: $(SRC)
	rm -rf build/classes && mkdir -p build/classes
	javac --release 22 -Xlint:all -Werror -cp $(API_CP) -d build/classes $(SRC)
	jar --create --file $@ -C build/classes .

test: build/warpspan-otel.jar capture
	rm -rf build/test-classes && mkdir -p build/test-classes
	javac --release 22 -cp build/classes:$(TEST_CP) -d build/test-classes $(TEST)
	$(JAVA_RUN) io.github.zraisan.warpspan.WarpspanTest $(LIB)
	$(JAVA_RUN) io.github.zraisan.warpspan.WarpspanTest $(LIB) --late

capture:
	$(MAKE) -C ../../capture

clean:
	rm -rf build

.PHONY: jar test capture clean
