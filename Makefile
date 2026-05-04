.PHONY: dev build install test lint clean gx10-worker gx10-bundle swagger

# Pinned to match the runtime swaggo/* deps in go.mod. Bump in lockstep.
SWAG_VERSION := v1.8.1
SWAG := $(shell go env GOPATH)/bin/swag

dev:
	./scripts/dev.sh

build: swagger gx10-bundle
	./scripts/build.sh

# swagger regenerates the OpenAPI 3 spec from swaggo annotations on the
# handler funcs. Output lands in internal/api/openapi/ and is committed to
# the repo so SwaggerUI works in any checkout without `swag` installed.
# `make build` runs this automatically; rerun manually after annotating
# new handlers, then commit the regenerated docs.go + swagger.{json,yaml}.
swagger:
	@command -v $(SWAG) >/dev/null 2>&1 || \
		go install github.com/swaggo/swag/cmd/swag@$(SWAG_VERSION)
	$(SWAG) init -g cmd/server/main.go -o internal/api/openapi --parseInternal

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
