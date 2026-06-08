ARG BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${BASE_IMAGE}

# CPython 3.13 free-threaded (no-GIL) via deadsnakes `python3.13-nogil`.
# The free-threaded interpreter binary is `python3.13t`.
# Do NOT override ENTRYPOINT/CMD: cubesandbox-base's entrypoint launches envd.
ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
 && apt-get install -y --no-install-recommends software-properties-common gnupg ca-certificates curl \
 && add-apt-repository -y ppa:deadsnakes/ppa \
 && apt-get update \
 && apt-get install -y --no-install-recommends python3.13-nogil \
 && curl -fsSL https://bootstrap.pypa.io/get-pip.py | python3.13t \
 && ln -sf /usr/bin/python3.13t /usr/local/bin/python3 \
 && ln -sf /usr/bin/python3.13t /usr/local/bin/python \
 && apt-get clean && rm -rf /var/lib/apt/lists/* \
 && python3 -VV
