.PHONY: dev build install test lint clean gx10-worker gx10-bundle

dev:
	./scripts/dev.sh

build: gx10-bundle
	./scripts/build.sh

install:
	sudo ./scripts/install.sh

# gx10-worker cross-compiles the GPU job worker for ARM64 (the GX10's
# native arch). Output lands in scripts/gx10/ for source-tree dev runs,
# AND in cmd/server/gx10-assets/ so the production build embeds it via
# //go:embed.
gx10-worker:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build \
		-trimpath -ldflags="-s -w" \
		-o scripts/gx10/gx10-worker ./cmd/gx10-worker
	@echo "built $$(file scripts/gx10/gx10-worker | cut -d, -f2)"

# gx10-bundle stages the install scripts + worker binary into the
# embed directory so `go build` picks them up via //go:embed. Run this
# before any production build (`make build` does so automatically).
# Keeping the directory under cmd/server/ mirrors the frontend/dist
# embed pattern — both are build artifacts living next to the binary
# they're packaged into.
gx10-bundle: gx10-worker
	@mkdir -p cmd/server/gx10-assets
	@cp scripts/gx10/install-inference.sh \
	    scripts/gx10/install-worker.sh \
	    scripts/gx10/demo-mnist.py \
	    scripts/gx10/gx10-worker \
	    cmd/server/gx10-assets/
	@echo "staged $$(ls cmd/server/gx10-assets | wc -l) gx10 assets for embed"

test:
	go test ./...
	cd frontend && npm run type-check

lint:
	golangci-lint run
	cd frontend && npm run lint

clean:
	rm -rf cmd/server/frontend/dist nimbus nimbus-*
	rm -f scripts/gx10/gx10-worker
