# Convenience targets; each one runs the same commands as CI or the bench scripts.
# Go code lives in server/, Python code in client/ (managed with uv).

GO_FUZZ_TARGETS := FuzzParseRTP FuzzParseVideo FuzzParseFEC FuzzFECRecovery FuzzParseRegister \
                   FuzzParseRegisterAck FuzzControlFrame FuzzParseNACK
FUZZTIME ?= 30s
STATICCHECK ?= staticcheck

.PHONY: all build test go-test py-test lint fuzz e2e bench-b1 bench-b2 bench-b3 bench-micro render docker clean

all: build test

build:
	cd server && go build -o bin/confserver ./cmd/confserver && go build -o bin/loadgen ./cmd/loadgen

test: go-test py-test

go-test:
	cd server && go vet ./... && $(STATICCHECK) ./... && go test -race ./...

py-test:
	cd client && uv sync --frozen && uv run ruff check . && uv run ruff format --check . && uv run mypy && uv run pytest

lint:
	cd server && gofmt -l . | (! grep .) && go vet ./... && $(STATICCHECK) ./...
	cd client && uv run ruff check . ../bench && uv run ruff format --check . ../bench && uv run mypy

# Short native fuzzing runs, one target at a time (go test -fuzz accepts one target).
fuzz:
	cd server && for t in $(GO_FUZZ_TARGETS); do \
		nice -n 19 go test -run '^$$' -fuzz "^$$t$$" -fuzztime $(FUZZTIME) -fuzzminimizetime 100x ./internal/protocol || exit 1; \
	done

e2e:
	cd client && uv run pytest -m e2e -v

# Benchmarks (B1-B3 need root for network namespaces and nftables).
bench-b1:
	sudo bench/run_b1.sh

bench-b2:
	sudo bench/run_b2.sh

bench-b3:
	sudo bench/run_b3.sh
	sudo MTU=1500 bench/run_b3.sh

bench-micro:
	bench/run_micro.sh

render:
	cd client && uv run --group bench python ../bench/render.py

docker:
	docker build -t confserver:dev -f server/Dockerfile server

clean:
	rm -rf server/bin bench/tmp
