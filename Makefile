.PHONY: benchmark
benchmark:
	go test -bench . ./...

.PHONY: cover
cover:
	go tool cover -html cover.out

.PHONY: fmt
fmt:
	goimports -w -local `head -n 1 go.mod | sed 's/^module //'` .

.PHONY: lint
lint:
	golangci-lint run

.PHONY: test
test:
	go test -coverprofile cover.out -shuffle on ./...

# Replay one simulator seed with a full trace. Optional: WORKLOAD=bank, MUTATION=cas|gc|freshness|ack.
.PHONY: replay
replay:
	MARAGUDB_SEED=$(SEED) MARAGUDB_WORKLOAD=$(WORKLOAD) MARAGUDB_MUTATION=$(MUTATION) go test ./internal/sim -run 'TestSimulator/replay' -count 1 -v

# Run the simulator with random seeds and configurations for SOAK_DURATION (default 5m).
SOAK_DURATION ?= 5m
.PHONY: soak
soak:
	MARAGUDB_SOAK=1 MARAGUDB_SOAK_DURATION=$(SOAK_DURATION) go test ./internal/sim -run 'TestSimulator/soak' -count 1 -v -timeout 0

# Run the Postgres differential harness with many more cases (default 3000).
PGDIFF_CASES ?= 3000
.PHONY: pgdiff
pgdiff:
	MARAGUDB_PGDIFF_CASES=$(PGDIFF_CASES) go test ./internal/pgdiff -run TestDifferential -count 1 -v -timeout 0

# Run only the Tigris integration suite.
.PHONY: tigris
tigris:
	go test ./internal/s3 ./internal/engine -run Tigris -count 1 -v
