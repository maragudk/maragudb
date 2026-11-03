---
sut_path: /
updated: 2026-11-03
---

# Deployment topology

Two containers, in `/antithesis/config/docker-compose.yaml`:

- `maragudb-s3`: versitygw v1.10.0 with its POSIX backend, the S3-compatible store. The bucket `maragudb` is a directory created at startup. Uninstrumented: it is a dependency, not the system under test, and its image is Alpine-based. Its conditional writes (`If-Match`, `If-None-Match: *`, `If-None-Match` on reads) match S3 and the in-process fake, and stayed atomic in a local stress test with 16 concurrent writers per object.
- `maragudb-workload`: the instrumented workload binary, built from `/antithesis/workload` by `/antithesis/Dockerfile`, cross-compiled to linux/amd64 with cgo. Its entrypoint, `maragudb-workload ready`, waits until the store answers, emits `setup_complete` through the SDK, and idles. The test commands are symlinks to the same binary under `/opt/antithesis/test/v1/maragudb/`, and the binary picks its mode from the name it runs under.

Every client of the database is a goroutine in the workload process with an engine of its own, so they behave like separate processes towards the store. Antithesis faults reach them through the network between the two containers (partitions, latency, resets) and through pausing or killing either container.

## Why a real store, and the fake as well

There are two kinds of test command.

- `parallel_driver_s3_*` run the bank and list-append workloads through the engine and through SQL against versitygw over the network. Here Antithesis's own faults are what the run explores: a partition in the middle of a manifest write is a real lost answer, a killed store is a real outage, and the AWS SDK client, its retries, and its error mapping are in the path. No in-process fake can show those, because faults injected on the network do not reach a store in the same process.
- `parallel_driver_sim` runs the deterministic simulator with the in-process fake, with seeds and configurations drawn from the SDK's random source. It covers what the network cannot inject on demand: crashes at every step, including mid-commit and mid-compaction, lost acknowledgements, and outages, all reproducible from the seed.

MinIO was not an option: its images are no longer published on Docker Hub.
