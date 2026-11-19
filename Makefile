# Convenience targets. Go code lives in server/, Python code in client/ (managed with uv).
#
# The CI workflow (.github/workflows/ci.yml) runs one ci-* target per step, so
# `make ci` runs the steps of its Linux jobs on this machine, in the same order:
# lint, Go tests, Python tests on 3.11 and 3.13, and the container image. It replaces
# client/.venv (once per Python version), so run it in a fresh clone:
#   git clone --no-local . /tmp/ci-clone && make -C /tmp/ci-clone ci
# The macOS and Windows jobs run plain go and pytest commands, listed in the workflow.

GO_FUZZ_TARGETS := FuzzParseRTP FuzzParseVideo FuzzParseFEC FuzzFECRecovery FuzzParseRegister \
                   FuzzParseRegisterAck FuzzControlFrame FuzzParseNACK
FUZZTIME ?= 30s
CI_FUZZTIME ?= 10s
# staticcheck 2025.1.1, as in CI. `go install honnef.co/go/tools/cmd/staticcheck@2025.1.1`
# puts the same version on PATH; then STATICCHECK=staticcheck saves the go run step.
STATICCHECK ?= go run honnef.co/go/tools/cmd/staticcheck@2025.1.1
# Targets of the cross-platform vet and build step (no recvmmsg/sendmmsg there).
CROSS_TARGETS := darwin/arm64 windows/amd64 linux/386
PYTHON ?= 3.13
CI_PYTHONS := 3.11 3.13
# Extra docker build flags, e.g. a proxy's CA bundle as a build secret (server/Dockerfile).
DOCKER_BUILD_FLAGS ?=

.PHONY: all build test lint go-test py-test fuzz e2e bench-b1 bench-b2 bench-b3 bench-micro render \
	docker clean ci ci-lint ci-go ci-python ci-docker ci-gofmt ci-vet ci-staticcheck ci-cross \
	ci-py-lint-install ci-ruff ci-mypy ci-race ci-fuzz ci-bench ci-py-install ci-pytest ci-pytest-unit \
	ci-docker-build ci-docker-smoke

all: build test

build:
	cd server && go build -o bin/confserver ./cmd/confserver && go build -o bin/loadgen ./cmd/loadgen

# Tests run after the linters, as in CI.
test: lint
	$(MAKE) go-test py-test

lint: ci-gofmt ci-vet ci-staticcheck ci-ruff ci-mypy

go-test: ci-race

py-test:
	cd client && uv sync --frozen && uv run pytest

# Short native fuzzing runs, one target at a time (go test -fuzz accepts one target).
fuzz:
	cd server && for t in $(GO_FUZZ_TARGETS); do \
		nice -n 19 go test -run '^$$' -fuzz "^$$t$$" -fuzztime $(FUZZTIME) -fuzzminimizetime 100x ./internal/protocol || exit 1; \
	done

e2e:
	cd client && uv run pytest -m e2e -v

# ---- CI steps ----------------------------------------------------------------------

ci: ci-lint ci-go ci-python ci-docker

ci-lint: ci-gofmt ci-vet ci-staticcheck ci-cross ci-py-lint-install ci-ruff ci-mypy

ci-go: ci-race ci-fuzz ci-bench

ci-python:
	for v in $(CI_PYTHONS); do $(MAKE) ci-py-install ci-pytest PYTHON=$$v || exit 1; done

ci-docker: ci-docker-build ci-docker-smoke

ci-gofmt:
	cd server && test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)

ci-vet:
	cd server && go vet ./...

ci-staticcheck:
	cd server && $(STATICCHECK) ./...

ci-cross:
	cd server && for target in $(CROSS_TARGETS); do \
		GOOS=$${target%/*} GOARCH=$${target#*/} go vet ./... || exit 1; \
		GOOS=$${target%/*} GOARCH=$${target#*/} go build ./... || exit 1; \
	done

# The linters see bench/ too, whose plotting script needs the bench group (matplotlib).
ci-py-lint-install:
	cd client && uv sync --frozen --group bench --python $(PYTHON)

ci-ruff:
	cd client && uv run --frozen ruff check . ../bench && uv run --frozen ruff format --check . ../bench

ci-mypy:
	cd client && uv run --frozen mypy

ci-race:
	cd server && go test -race ./...

# -fuzzminimizetime 100x: with the default (60 s per input), FuzzParseVideo and
# FuzzFECRecovery spent most of a 10 s run minimizing inputs whose new coverage did not
# reproduce, and stopped executing (GODEBUG=fuzzdebug=1 shows it).
ci-fuzz:
	cd server && for t in $(GO_FUZZ_TARGETS); do \
		go test -run '^$$' -fuzz "^$$t$$" -fuzztime $(CI_FUZZTIME) -fuzzminimizetime 100x ./internal/protocol || exit 1; \
	done

ci-bench:
	cd server && go test -run '^$$' -bench . -benchtime 1x ./...

ci-py-install:
	cd client && uv sync --frozen --python $(PYTHON)

# Unit and end-to-end tests; the e2e tests build and start the Go server.
ci-pytest:
	cd client && uv run --frozen pytest -v

ci-pytest-unit:
	cd client && uv run --frozen pytest -v -m "not e2e"

ci-docker-build:
	docker build $(DOCKER_BUILD_FLAGS) -t confserver:ci -f server/Dockerfile server

# Flags after the image name are appended; HEALTHCHECK completes a hello.
ci-docker-smoke:
	docker rm -f confserver-ci >/dev/null 2>&1 || true
	docker run -d --name confserver-ci --health-interval 1s \
		-p 127.0.0.1:6100:6100/tcp -p 127.0.0.1:6100:6100/udp -p 127.0.0.1:6101:6101 \
		confserver:ci -log-format json -p2p=false
	status=0; \
	for i in $$(seq 1 30); do \
		[ "$$(docker inspect -f '{{.State.Health.Status}}' confserver-ci)" = healthy ] && break; \
		sleep 1; \
	done; \
	docker inspect -f '{{.State.Health.Status}}' confserver-ci | grep -qx healthy && \
	curl -fsS --noproxy '*' http://127.0.0.1:6101/metrics | grep -q '^confserver_rooms' && \
	docker logs confserver-ci 2>&1 | head -n 1 | grep -q '^{' || status=1; \
	docker rm -f confserver-ci >/dev/null; \
	exit $$status

# ---- Benchmarks (B1-B3 need root for network namespaces and nftables) ---------------

bench-b1:
	sudo bench/run_b1.sh

bench-b2:
	sudo bench/run_b2.sh

bench-b3:
	sudo bench/run_b3.sh

bench-micro:
	bench/run_micro.sh

render:
	cd client && uv run --group bench python ../bench/render.py

docker:
	docker build $(DOCKER_BUILD_FLAGS) -t confserver:dev -f server/Dockerfile server

clean:
	rm -rf server/bin bench/tmp
