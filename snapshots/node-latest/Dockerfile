ARG BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${BASE_IMAGE}

# Node.js current release via NodeSource.
# Do NOT override ENTRYPOINT/CMD: cubesandbox-base's entrypoint launches envd.
ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
 && apt-get install -y --no-install-recommends curl ca-certificates gnupg \
 && curl -fsSL https://deb.nodesource.com/setup_current.x | bash - \
 && apt-get install -y --no-install-recommends nodejs \
 && apt-get clean && rm -rf /var/lib/apt/lists/* \
 && node --version && npm --version
