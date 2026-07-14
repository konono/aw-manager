FROM golang:1.25 AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /aw-manager .

# Build aw from source (manifest subcommand is not yet in a published release)
RUN git clone --depth 1 https://github.com/konono/aw.git /tmp/aw && \
    cd /tmp/aw && CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /aw .

FROM debian:bookworm-slim

RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates git && \
    rm -rf /var/lib/apt/lists/*

RUN (getent passwd 1001 | cut -d: -f1 | xargs -r userdel) 2>/dev/null; \
    useradd -m -s /bin/bash -u 1001 -g 0 agent && \
    chgrp -R 0 /home/agent && chmod -R g=u /home/agent

COPY --from=builder /aw-manager /usr/local/bin/aw-manager
COPY --from=builder /aw /usr/local/bin/aw

ENV HOME="/home/agent"

USER agent
WORKDIR /home/agent
ENTRYPOINT ["aw-manager"]
