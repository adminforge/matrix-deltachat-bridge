# Stage 1: Build deltachat-rpc-server (Rust)
FROM rust:1.94-slim-bookworm AS rust-builder

RUN apt-get update && apt-get install -y \
    cmake git pkg-config libssl-dev libsqlite3-dev build-essential \
    && rm -rf /var/lib/apt/lists/*

RUN cargo install --git https://github.com/deltachat/deltachat-core-rust/ deltachat-rpc-server

# Stage 2: Build Go Bridge
FROM golang:1.25-bookworm AS go-builder

# Install libolm for Matrix E2EE
RUN apt-get update && apt-get install -y \
    libolm-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /usr/src/bridge/src
COPY src/go.mod src/go.sum ./
RUN go mod download

WORKDIR /usr/src/bridge
COPY . .
WORKDIR /usr/src/bridge/src
RUN go build -o ../bridge .

# Final Stage
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y \
    libssl3 libsqlite3-0 libolm3 ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && echo "precedence ::ffff:0:0/96  100" > /etc/gai.conf

# Copy binaries
COPY --from=rust-builder /usr/local/cargo/bin/deltachat-rpc-server /usr/local/bin/
COPY --from=go-builder /usr/src/bridge/bridge /usr/local/bin/bridge

# Ensure deltachat-rpc-server is in PATH
ENV PATH="/usr/local/bin:${PATH}"

# Work in /data for persistence
WORKDIR /data

CMD ["bridge"]
