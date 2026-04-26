.PHONY: dev build install test lint clean gx10-worker

dev:
	./scripts/dev.sh

build:
	./scripts/build.sh

install:
	sudo ./scripts/install.sh

# gx10-worker cross-compiles the GPU job worker for ARM64 (the GX10's
# native arch) and drops the binary into scripts/gx10/ so the install
# script can fetch it via /api/gpu/scripts/gx10-worker. Build before
# committing changes to cmd/gx10-worker so deployments pick up the
# new binary.
gx10-worker:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build \
		-trimpath -ldflags="-s -w" \
		-o scripts/gx10/gx10-worker ./cmd/gx10-worker
	@echo "built $$(file scripts/gx10/gx10-worker | cut -d, -f2)"

test:
	go test ./...
	cd frontend && npm run type-check

lint:
	golangci-lint run
	cd frontend && npm run lint

clean:
	rm -rf cmd/server/frontend/dist nimbus nimbus-*
	rm -f scripts/gx10/gx10-worker
