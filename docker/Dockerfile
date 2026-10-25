FROM golang:1.26-alpine

# The image contains only the runtime and the startup script. The application
# source is cloned from GitHub at container start, exactly like the standard
# VPS template. A key advantage of this server is that it has no third-party
# Go modules, so the startup build is fast and does not depend on a network
# module proxy.
RUN apk add --no-cache \
        bash \
        ca-certificates \
        curl \
        file \
        git \
        jq \
        less \
        python3 \
        py3-pip \
        ripgrep \
        tar \
        tzdata \
        unzip \
    && addgroup -g 10001 minis \
    && adduser -D -u 10001 -G minis -s /bin/sh -h /home/minis minis \
    && mkdir -p /app /data/bin /data/app-cache /data/workspaces /home/minis

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENV GOTOOLCHAIN=local \
    CGO_ENABLED=0 \
    MINIS_CONFIG=/data/config.json \
    MINIS_LISTEN=:8080

WORKDIR /app
EXPOSE 8080

ENTRYPOINT ["/entrypoint.sh"]
CMD ["/data/bin/minis-server"]
