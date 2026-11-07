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

# Directory where the tests record verification facts, which `make report` renders.
VERIFY_DIR ?= .verify

# Run every test, and record the verification facts in VERIFY_DIR.
.PHONY: test
test:
	mkdir -p $(VERIFY_DIR) && find $(VERIFY_DIR) -name 'pkg-*.jsonl' -delete
	MARAGUDB_VERIFY_DIR=$(abspath $(VERIFY_DIR)) go test -coverprofile cover.out -shuffle on ./...

# Render the verification facts of the last `make test` and `make antithesis` as Markdown.
.PHONY: report
report:
	go run ./internal/verify/cmd/report -dir $(VERIFY_DIR)

# Regenerate the mutant kill matrix and docs/verification.md.
.PHONY: verification
verification:
	go test ./internal/sim -run 'TestMutants' -count 1 -args -update
	go test ./internal/verify -run 'TestVerification' -count 1 -args -update

# Replay one simulator seed with a full trace. Optional: WORKLOAD=bank, WORKLOAD=unique, or
# WORKLOAD=foreign-key, the last two of which run through SQL, API=sql, CORPUS=1 to use the
# corpus configuration of that seed, workload, and API, MAINTAINERS=<n> (default 2),
# MAINTENANCE=lease|none|overlap, DISK_FAULTS=1 for small disk caches whose files are damaged,
# STORE_DAMAGE=1 for reads whose bodies the store damages, and MUTATION=<name>, where the names
# are in the kill matrix in
# docs/verification.md: cas, gc, gc-batch-objects-first, gc-sweep-segments-only,
# gc-chain-first-stamp, gc-delete-referenced, compact-non-adjacent, compact-drop-tombstones,
# compact-rebase-loses-segment, compact-rebase-on-compaction, freshness, ack, compacted-ahead,
# fresh-snapshot, retry-unknown, index-stale-delete, unique-no-check, unique-own-writes,
# fk-no-child-check, fk-no-parent-check, fk-own-writes, check-skip-update, subquery-stale-reads,
# update-sees-own-writes, disk-cache-unverified, disk-cache-serve-cut-read, disk-cache-uncounted,
# segment-unchecked, and maintenance-unrecovered.
.PHONY: replay
replay:
	MARAGUDB_SEED=$(SEED) MARAGUDB_WORKLOAD=$(WORKLOAD) MARAGUDB_API=$(API) MARAGUDB_CORPUS=$(CORPUS) MARAGUDB_MAINTAINERS=$(MAINTAINERS) MARAGUDB_MAINTENANCE=$(MAINTENANCE) MARAGUDB_DISK_FAULTS=$(DISK_FAULTS) MARAGUDB_STORE_DAMAGE=$(STORE_DAMAGE) MARAGUDB_MUTATION=$(MUTATION) go test ./internal/sim -run 'TestSimulator/replay' -count 1 -v

# Run the mutant kill matrix with every seed in every cell, then the simulator with random seeds
# and configurations for SOAK_DURATION (default 5m), and then the full sqllogictest corpus.
SOAK_DURATION ?= 5m
.PHONY: soak
soak:
	MARAGUDB_SOAK=1 go test ./internal/sim -run 'TestMutants' -count 1 -timeout 0
	MARAGUDB_SOAK=1 MARAGUDB_SOAK_DURATION=$(SOAK_DURATION) go test ./internal/sim -run 'TestSimulator/soak' -count 1 -v -timeout 0
	$(MAKE) sqllogictest

# Run the SQLite sqllogictest corpus on Postgres and maragudb: all of it, or with
# SQLLOGICTEST_FILES the files that match comma-separated globs, such as
# SQLLOGICTEST_FILES='index/between/**,random/expr/slt_good_1*.test'. The corpus is downloaded
# into the user cache directory on first use.
.PHONY: sqllogictest
sqllogictest:
	MARAGUDB_SQLLOGICTEST=full MARAGUDB_SQLLOGICTEST_FILES='$(SQLLOGICTEST_FILES)' go test ./internal/sqllogictest -run 'TestCorpus' -count 1 -v -timeout 0

# Rewrite the baseline in /internal/sqllogictest/testdata/baseline.json: the number of
# statements of each corpus file that run on both sides, for the whole corpus or for
# SQLLOGICTEST_FILES. Commit a higher count; the test fails when a file's count drops.
.PHONY: sqllogictest-ratchet
sqllogictest-ratchet:
	MARAGUDB_SQLLOGICTEST=full MARAGUDB_SQLLOGICTEST_FILES='$(SQLLOGICTEST_FILES)' go test ./internal/sqllogictest -run 'TestCorpus' -count 1 -v -timeout 0 -args -update

# Rewrite the ratchet baseline in /internal/pgdiff/testdata/ratchet.json: the number of
# statements that a fixed-seed differential run executes on both sides. Commit a higher count;
# the test fails when the count drops.
.PHONY: ratchet
ratchet:
	go test ./internal/pgdiff -run 'TestDifferential/the_generator' -count 1 -args -update

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
	mkdir -p $(VERIFY_DIR) && : > $(VERIFY_DIR)/antithesis.jsonl
	status=0; \
	for c in $$($(ANTITHESIS_COMPOSE) exec -T workload ls /opt/antithesis/test/v1/maragudb); do \
		echo "== $$c"; \
		passed=true; \
		$(ANTITHESIS_COMPOSE) exec -T workload /opt/antithesis/test/v1/maragudb/$$c || { status=1; passed=false; }; \
		printf '{"package":"antithesis","antithesis":{"command":"%s","passed":%s}}\n' "$$c" "$$passed" >> $(VERIFY_DIR)/antithesis.jsonl; \
	done; \
	$(ANTITHESIS_COMPOSE) logs workload | grep -q "setup complete" || { echo "the workload never signaled setup complete"; status=1; }; \
	$(ANTITHESIS_COMPOSE) down -v; \
	exit $$status

# Validate the harness with snouty, the Antithesis CLI, which must be on PATH.
.PHONY: antithesis-validate
antithesis-validate: antithesis-build
	SNOUTY_CONTAINER_ENGINE=docker snouty validate antithesis/config
