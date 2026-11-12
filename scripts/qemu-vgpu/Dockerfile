FROM ubuntu:24.04 AS build
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential ca-certificates git python3 python3-venv python3-pip \
    ninja-build pkg-config libglib2.0-dev libpixman-1-dev zlib1g-dev libaio-dev \
    && rm -rf /var/lib/apt/lists/*
COPY scripts/qemu-vgpu /src
RUN bash /src/build.sh /runtime

FROM scratch AS artifact
COPY --from=build /runtime/ /
