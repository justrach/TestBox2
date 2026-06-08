ARG BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${BASE_IMAGE}

# Bun (latest) via the official install script.
# Do NOT override ENTRYPOINT/CMD: cubesandbox-base's entrypoint launches envd.
ENV DEBIAN_FRONTEND=noninteractive
ENV BUN_INSTALL=/usr/local
ENV PATH=/usr/local/bin:$PATH

RUN apt-get update \
 && apt-get install -y --no-install-recommends curl ca-certificates unzip \
 && curl -fsSL https://bun.sh/install | bash \
 && ln -sf /usr/local/bin/bun /usr/local/bin/bunx \
 && apt-get clean && rm -rf /var/lib/apt/lists/* \
 && bun --version
