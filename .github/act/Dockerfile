FROM ubuntu:26.04

RUN apt-get update && apt-get install -y --no-install-recommends \
    curl ca-certificates git \
    && rm -rf /var/lib/apt/lists/*

# Pin act version for reproducibility
# renovate: datasource=github-releases depName=nektos/act extractVersion=^v(?<version>.+)$
ARG ACT_VERSION=0.2.98
RUN curl -fsSL https://github.com/nektos/act/releases/download/v${ACT_VERSION}/act_Linux_x86_64.tar.gz \
    | tar -xz -C /usr/local/bin act

WORKDIR /workspace
ENTRYPOINT ["act"]
