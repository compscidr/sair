# Build stage
FROM golang:1.26 AS builder
ARG VERSION=dev

# Install protoc and Go plugins
RUN apt-get update && apt-get install -y --no-install-recommends protobuf-compiler && rm -rf /var/lib/apt/lists/*
RUN go install google.golang.org/protobuf/cmd/protoc-gen-go@latest && \
    go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN make proto
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X github.com/compscidr/sair/internal/version.Version=${VERSION}" -o /bin/sair-device-source ./cmd/sair-device-source
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X github.com/compscidr/sair/internal/version.Version=${VERSION}" -o /bin/sair-proxy ./cmd/sair-proxy

# Device source image
# NOTE: The real adb server must run on the host (not in this container).
# This container connects to the host's adb server via ADB_PORT.
FROM alpine:3.23 AS device-source
RUN apk add --no-cache android-tools
COPY --from=builder /bin/sair-device-source /usr/local/bin/sair-device-source
RUN adduser -D -u 1000 sair
USER sair
EXPOSE 8080
ENTRYPOINT ["sair-device-source"]

# Proxy image
FROM alpine:3.23 AS proxy
COPY --from=builder /bin/sair-proxy /usr/local/bin/sair-proxy
RUN adduser -D -u 1000 sair
USER sair
EXPOSE 5037 8550
ENTRYPOINT ["sair-proxy"]
