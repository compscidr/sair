.PHONY: proto build clean

# Stubs are committed; regenerate after editing a .proto. Plugin versions are
# pinned by the tool directives in go.mod, protoc by .github/actions/install-protoc.
proto:
	go install tool
	protoc \
		--go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		proto/devicesource/devicesource.proto
	protoc \
		--go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		proto/orchestrator/orchestrator.proto

VERSION ?= dev

build:
	go build -ldflags="-X github.com/compscidr/sair/internal/version.Version=$(VERSION)" ./cmd/sair-device-source
	go build -ldflags="-X github.com/compscidr/sair/internal/version.Version=$(VERSION)" ./cmd/sair-proxy

clean:
	rm -f sair-device-source sair-proxy
