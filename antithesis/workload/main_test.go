package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/verify"
)

func TestMain(m *testing.M) {
	verify.Main(m)
}

type statusError int

func (e statusError) Error() string       { return fmt.Sprint("status ", int(e)) }
func (e statusError) HTTPStatusCode() int { return int(e) }

func TestCommands(t *testing.T) {
	t.Run("every test command of the harness runs a command of the workload, against the store the compose file runs", func(t *testing.T) {
		verify.Covers(t, "every test command in /antithesis/test/maragudb has a valid prefix and names a workload command, and the compose file runs versitygw and the workload image", "AC-8.2")
		entries, err := os.ReadDir("../test/maragudb")
		is.NotError(t, err)
		is.True(t, len(entries) >= 5, "the template has the test commands")
		for _, e := range entries {
			is.True(t, strings.HasPrefix(e.Name(), "parallel_driver_"), e.Name())
			_, ok := commands[commandOf(e.Name())]
			is.True(t, ok, e.Name()+" runs a workload command")
		}
		compose, err := os.ReadFile("../config/docker-compose.yaml")
		is.NotError(t, err)
		for _, want := range []string{"image: docker.io/versity/versitygw:", "dockerfile: antithesis/Dockerfile", "AWS_ENDPOINT_URL_S3: http://maragudb-s3:7070"} {
			is.True(t, strings.Contains(string(compose), want), want)
		}
	})

	t.Run("the sim command runs simulator seeds without anomalies", func(t *testing.T) {
		verify.Covers(t, "the sim test command's simulation runs eight seeds with random configurations, drawn here from a fixed source, and reports no anomalies", "AC-8.2")
		_, ok := commands["sim"]
		is.True(t, ok)
		is.NotError(t, simulate(rand.NewPCG(1, 2)))
	})

	t.Run("the sim command turns on disk faults and damaged reads from the store in some runs and not in others", func(t *testing.T) {
		verify.Covers(t, "of the configurations that the sim test command draws for 400 seeds, between a sixth and a third have disk faults, between a sixth and a third have damaged reads from the store, and some have both", "AC-8.2")
		var disk, damage, both int
		for seed := range uint64(400) {
			cfg := simConfig(seed)
			if cfg.DiskFaults {
				disk++
			}
			if cfg.StoreDamage {
				damage++
			}
			if cfg.DiskFaults && cfg.StoreDamage {
				both++
			}
		}
		is.True(t, disk > 400/6 && disk < 400/3, fmt.Sprint(disk))
		is.True(t, damage > 400/6 && damage < 400/3, fmt.Sprint(damage))
		is.True(t, both > 0)
	})

	t.Run("the workload cross-compiles to linux/amd64", func(t *testing.T) {
		verify.Covers(t, "go build of the workload for GOOS=linux GOARCH=amd64 succeeds", "AC-8.3")
		cmd := exec.CommandContext(t.Context(), "go", "build", "-o", filepath.Join(t.TempDir(), "workload"), ".")
		cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
		out, err := cmd.CombinedOutput()
		is.NotError(t, err, string(out))
	})
}

func TestUnreachable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"a timeout", fmt.Errorf("engine: %w", context.DeadlineExceeded), true},
		{"a network error", fmt.Errorf("s3: %w", &net.OpError{Op: "dial", Err: errors.New("connection refused")}), true},
		{"a server error", fmt.Errorf("s3: %w", statusError(503)), true},
		{"a client error", fmt.Errorf("s3: %w", statusError(403)), false},
		{"anything else", errors.New("sql: invalid statement"), false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := unreachable(test.err); got != test.want {
				t.Fatalf("unreachable(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}
