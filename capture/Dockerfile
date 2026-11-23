# "Inject" image: contains libwarpspan.so, the libcupti it was built against and
# the Python span bridge. An init container copies /warpspan into a shared
# volume of the app pod (see deploy/kubernetes/example-gpu-pod.yaml).
#
# Build from the repo root. Pick a CUDA_IMAGE whose CUDA major version matches
# your nodes' driver (nvidia-smi shows "CUDA Version"). An older Ubuntu base keeps
# the glibc requirement low so the .so loads in more app images.
#   docker build -f capture/Dockerfile -t warpspan-inject \
#     --build-arg CUDA_IMAGE=nvidia/cuda:12.6.3-devel-ubuntu20.04 .
ARG CUDA_IMAGE=nvidia/cuda:12.6.3-devel-ubuntu20.04
FROM ${CUDA_IMAGE} AS build
# STATIC_CXX=0 if the toolchain's libstdc++.a is not built with -fPIC.
ARG STATIC_CXX=1
RUN apt-get update && apt-get install -y --no-install-recommends make g++ && rm -rf /var/lib/apt/lists/*
WORKDIR /src/capture
COPY capture/ .
RUN make CUDA_HOME=/usr/local/cuda STATIC_CXX=${STATIC_CXX}
# Ship the exact libcupti soname the .so needs next to it (found via RUNPATH $ORIGIN),
# because most app images contain the CUDA runtime but not CUPTI.
RUN mkdir -p /out/python \
 && cp libwarpspan.so /out/ \
 && soname="$(readelf -d libwarpspan.so | sed -n 's/.*NEEDED.*\[\(libcupti[^]]*\)\].*/\1/p')" \
 && cupti="$(find -L /usr/local/cuda/ -name "$soname" | head -1)" \
 && test -n "$cupti" && cp -L "$cupti" /out/"$soname"
COPY bridge/python/src/warpspan /out/python/warpspan
COPY bridge/python/autoload/sitecustomize.py /out/python/sitecustomize.py

FROM busybox:1.37
COPY --from=build /out /warpspan
CMD ["cp", "-a", "/warpspan/.", "/target/"]
