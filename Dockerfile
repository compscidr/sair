# Build stage
FROM golang:1.24 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /bin/sair-device-source ./cmd/sair-device-source
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /bin/sair-proxy ./cmd/sair-proxy

# Device source image
# NOTE: The real adb server must run on the host (not in this container).
# This container connects to the host's adb server via ADB_PORT.
FROM alpine:3.21 AS device-source
RUN apk add --no-cache android-tools
COPY --from=builder /bin/sair-device-source /usr/local/bin/sair-device-source
RUN adduser -D -u 1000 sair
USER sair
EXPOSE 8080
ENTRYPOINT ["sair-device-source"]

# Proxy image
FROM alpine:3.21 AS proxy
COPY --from=builder /bin/sair-proxy /usr/local/bin/sair-proxy
RUN adduser -D -u 1000 sair
USER sair
EXPOSE 5037 8550
ENTRYPOINT ["sair-proxy"]
