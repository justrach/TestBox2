ARG BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${BASE_IMAGE}

# Zig 0.16.0 — official static tarball from ziglang.org.
# Do NOT override ENTRYPOINT/CMD: cubesandbox-base's entrypoint launches envd.
ARG ZIG_VERSION=0.16.0
ARG ZIG_SHA256=70e49664a74374b48b51e6f3fdfbf437f6395d42509050588bd49abe52ba3d00
ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
 && apt-get install -y --no-install-recommends curl xz-utils ca-certificates \
 && curl -fsSL -o /tmp/zig.tar.xz \
      "https://ziglang.org/download/${ZIG_VERSION}/zig-x86_64-linux-${ZIG_VERSION}.tar.xz" \
 && echo "${ZIG_SHA256}  /tmp/zig.tar.xz" | sha256sum -c - \
 && mkdir -p /opt/zig \
 && tar -xJf /tmp/zig.tar.xz -C /opt/zig --strip-components=1 \
 && ln -sf /opt/zig/zig /usr/local/bin/zig \
 && rm -f /tmp/zig.tar.xz \
 && apt-get clean && rm -rf /var/lib/apt/lists/* \
 && zig version
