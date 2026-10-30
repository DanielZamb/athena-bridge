.PHONY: build test vet verify fmt install smoke secrets source release

build:
	CGO_ENABLED=0 go build -trimpath -o bin/ ./cmd/athena-bridge

test:
	go test -race ./...
	python3 -m unittest discover -s sandbox -p 'test_*.py'

vet:
	go vet ./...

verify: test vet build

fmt:
	gofmt -w cmd

install:
	CGO_ENABLED=0 go install -trimpath ./cmd/athena-bridge

smoke: build
	python3 scripts/verify-local.py

secrets:
	python3 scripts/check-secrets.py

release:
	python3 scripts/package-release.py

source:
	@if ! git diff --quiet HEAD -- || [ -n "$$(git ls-files --others --exclude-standard)" ]; then \
		printf '%s\n' 'Commit the reviewed public files before running make source.' >&2; \
		exit 1; \
	fi
	mkdir -p dist
	git archive --format=tar.gz --prefix=athena-bridge/ --output=dist/athena-bridge-source.tar.gz HEAD
