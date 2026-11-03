# syntax=docker/dockerfile:1

# The workload image for the Antithesis harness. Go cross-compiles the instrumented workload to
# linux/amd64 on the build machine's own architecture, with cgo, which the Antithesis SDK needs.
FROM --platform=$BUILDPLATFORM golang:1.27-trixie AS build
RUN if [ "$(dpkg --print-architecture)" != amd64 ]; then \
      apt-get update && apt-get install -y --no-install-recommends gcc-x86-64-linux-gnu libc6-dev-amd64-cross && rm -rf /var/lib/apt/lists/*; \
    fi
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
RUN go install github.com/antithesishq/antithesis-sdk-go/tools/antithesis-go-toolexec@v0.8.0
COPY . .
ENV CGO_ENABLED=1 GOOS=linux GOARCH=amd64
RUN if [ "$(dpkg --print-architecture)" != amd64 ]; then export CC=x86_64-linux-gnu-gcc; fi && \
    go build -toolexec=antithesis-go-toolexec -o /out/maragudb-workload ./antithesis/workload

FROM debian:stable-slim AS workload
ENV NO_COLOR=1
COPY --from=build /out/maragudb-workload /usr/local/bin/maragudb-workload
COPY --from=build /symbols /symbols
# The test commands are symlinks to the workload in the repository, which picks its mode from the
# name it runs under. They become hard links here, so that they are files of their own for tools
# that copy the test directory out of the image.
COPY antithesis/test /opt/antithesis/test/v1
RUN for f in /opt/antithesis/test/v1/*/*; do ln -f /usr/local/bin/maragudb-workload "$f"; done
ENTRYPOINT ["/usr/local/bin/maragudb-workload", "ready"]
