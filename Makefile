.PHONY: dev build install test lint clean

dev:
	./scripts/dev.sh

build:
	./scripts/build.sh

install:
	sudo ./scripts/install.sh

test:
	go test ./...
	cd frontend && npm run type-check

lint:
	golangci-lint run
	cd frontend && npm run lint

clean:
	rm -rf cmd/server/frontend/dist nimbus nimbus-*
