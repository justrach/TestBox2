ARG BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${BASE_IMAGE}

# CPython 3.10 via the deadsnakes PPA (built for the base's Ubuntu release).
# Do NOT override ENTRYPOINT/CMD: cubesandbox-base's entrypoint launches envd.
ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
 && apt-get install -y --no-install-recommends software-properties-common gnupg ca-certificates curl \
 && add-apt-repository -y ppa:deadsnakes/ppa \
 && apt-get update \
 && apt-get install -y --no-install-recommends python3.10 python3.10-venv python3.10-dev \
 && curl -fsSL https://bootstrap.pypa.io/get-pip.py | python3.10 \
 && ln -sf /usr/bin/python3.10 /usr/local/bin/python3 \
 && ln -sf /usr/bin/python3.10 /usr/local/bin/python \
 && apt-get clean && rm -rf /var/lib/apt/lists/* \
 && python3 --version
