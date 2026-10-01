FUZZTIME ?= 30s

.PHONY: build test cover lint fuzz crashtest sqltest bench check

build:
	go build -o bin/novacdb ./cmd/novacdb

test:
	go test -race -count=1 ./...

cover:
	go test -count=1 -cover ./...

lint:
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed on:"; gofmt -l .; exit 1; }
	go vet ./...
	golangci-lint run

# Runs every Fuzz target for FUZZTIME each. Minimising a new input is capped
# at FUZZMINIMIZE: Go's 60s default stalls fuzzing of page-sized inputs.
FUZZMINIMIZE ?= 5s
fuzz:
	@targets=$$(grep -rl --include='*_test.go' '^func Fuzz' . 2>/dev/null); \
	if [ -z "$$targets" ]; then echo "fuzz: no fuzz targets yet (first arrive in Step 1.1)"; exit 0; fi; \
	for f in $$targets; do \
		dir=$$(dirname $$f); \
		for t in $$(grep -h '^func Fuzz' $$f | sed 's/func \(Fuzz[A-Za-z0-9_]*\).*/\1/'); do \
			echo "fuzzing $$dir $$t for $(FUZZTIME)"; \
			go test $$dir -run='^$$' -fuzz="^$$t$$" -fuzztime=$(FUZZTIME) -fuzzminimizetime=$(FUZZMINIMIZE) || exit 1; \
		done; \
	done

# Crash and recovery tests with many more runs than `make test`:
# thousands of seeded MemFS crash scenarios and real processes killed with
# SIGKILL. Reproduce a failure with the seed it prints:
#   NOVACDB_SEED=<seed> NOVACDB_CRASH_RUNS=1 go test -run CrashRecoveryMemFS ./tests/crash
CRASH_RUNS ?= 3000
OSKILL_RUNS ?= 30
crashtest:
	NOVACDB_CRASH_RUNS=$(CRASH_RUNS) NOVACDB_OSKILL_RUNS=$(OSKILL_RUNS) go test -count=1 -timeout 60m ./tests/crash/...

sqltest:
	@echo "sqltest: SQL logic test runner not built yet (arrives in Step 4.6)"

bench:
	go test -run='^$$' -bench=. -benchmem ./...

check:
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed on:"; gofmt -l .; exit 1; }
	go vet ./...
	go build -o /dev/null ./...
	go test -race -count=1 ./...
	golangci-lint run
	$(MAKE) fuzz FUZZTIME=$(FUZZTIME)
	$(MAKE) crashtest
	go mod tidy
	@git diff --exit-code go.mod || { echo "go.mod changed after tidy"; exit 1; }
