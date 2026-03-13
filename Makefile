.PHONY: proto build clean

proto:
	protoc \
		--go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		proto/devicesource/devicesource.proto
	protoc \
		--go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		proto/orchestrator/orchestrator.proto

VERSION ?= dev

build: proto
	go build -ldflags="-X github.com/compscidr/sair/internal/version.Version=$(VERSION)" ./cmd/sair-device-source
	go build -ldflags="-X github.com/compscidr/sair/internal/version.Version=$(VERSION)" ./cmd/sair-proxy

clean:
	rm -f sair-device-source sair-proxy
	rm -f proto/devicesource/*.pb.go
	rm -f proto/orchestrator/*.pb.go
