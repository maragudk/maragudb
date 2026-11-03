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

# Replay one simulator seed with a full trace. Optional: WORKLOAD=bank, API=sql,
# MUTATION=cas|gc|freshness|ack|retry-unknown|fresh-snapshot.
.PHONY: replay
replay:
	MARAGUDB_SEED=$(SEED) MARAGUDB_WORKLOAD=$(WORKLOAD) MARAGUDB_API=$(API) MARAGUDB_MUTATION=$(MUTATION) go test ./internal/sim -run 'TestSimulator/replay' -count 1 -v

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

ANTITHESIS_COMPOSE = docker compose -f antithesis/config/docker-compose.yaml

# Build the Antithesis harness images in /antithesis for linux/amd64.
.PHONY: antithesis-build
antithesis-build:
	$(ANTITHESIS_COMPOSE) build

# Run the Antithesis harness locally without Antithesis: bring it up, run every test command
# once, check that setup completed, and tear it down.
.PHONY: antithesis
antithesis: antithesis-build
	$(ANTITHESIS_COMPOSE) up -d --wait
	status=0; \
	for c in $$($(ANTITHESIS_COMPOSE) exec -T workload ls /opt/antithesis/test/v1/maragudb); do \
		echo "== $$c"; \
		$(ANTITHESIS_COMPOSE) exec -T workload /opt/antithesis/test/v1/maragudb/$$c || status=1; \
	done; \
	$(ANTITHESIS_COMPOSE) logs workload | grep -q "setup complete" || { echo "the workload never signaled setup complete"; status=1; }; \
	$(ANTITHESIS_COMPOSE) down -v; \
	exit $$status

# Validate the harness with snouty, the Antithesis CLI, which must be on PATH.
.PHONY: antithesis-validate
antithesis-validate: antithesis-build
	SNOUTY_CONTAINER_ENGINE=docker snouty validate antithesis/config
