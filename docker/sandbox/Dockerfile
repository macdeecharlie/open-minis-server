# Dedicated full-distro execution environment. The server container never runs
# the agent's shell; it forwards tool calls here over a private Docker network.
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY app/ /src/
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/minis-server .

FROM debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive \
    LANG=C.UTF-8 \
    MINIS_ROLE=sandbox \
    MINIS_SANDBOX_LISTEN=:8090 \
    MINIS_SANDBOX_WORK_ROOT=/workspace/sessions

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        bash \
        build-essential \
        ca-certificates \
        curl \
        dnsutils \
        file \
        git \
        iproute2 \
        jq \
        less \
        nano \
        netcat-openbsd \
        nodejs \
        npm \
        openssh-client \
        pkg-config \
        procps \
        python3 \
        python3-pip \
        python3-venv \
        ripgrep \
        sqlite3 \
        tar \
        tzdata \
        unzip \
        vim-tiny \
        wget \
        xz-utils \
        zip \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /workspace/sessions /root

COPY --from=build /out/minis-server /usr/local/bin/minis-server

WORKDIR /workspace
EXPOSE 8090

CMD ["/usr/local/bin/minis-server", "-role", "sandbox"]
