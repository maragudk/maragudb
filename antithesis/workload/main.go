// Command workload is the maragudb workload for Antithesis. It runs as the test commands of the
// harness in /antithesis, chosen by the name it is invoked under, and as the entrypoint that
// waits for the object store and signals that setup is complete.
//
//	ready                          wait for the object store, signal setup complete, and idle
//	parallel_driver_sim            run simulator seeds against the in-process fake
//	parallel_driver_s3_<w>_<api>   run workload w (bank, list_append) through api (engine, sql)
//	                               against the object store
//
// The object store comes from AWS_ENDPOINT_URL_S3, BUCKET, and the standard AWS credentials.
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	mathrand "math/rand"
	"math/rand/v2"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/antithesishq/antithesis-sdk-go/assert"
	"github.com/antithesishq/antithesis-sdk-go/lifecycle"
	"github.com/antithesishq/antithesis-sdk-go/random"

	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/s3"
	"maragu.dev/maragudb/internal/sim"
	"maragu.dev/maragudb/internal/workload"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	name := filepath.Base(os.Args[0])
	if len(os.Args) > 1 {
		name = os.Args[1]
	}
	if err := run(ctx, commandOf(name)); err != nil {
		fmt.Println("workload:", err)
		os.Exit(1)
	}
}

// commandOf the name the workload is invoked under: the name without its test command prefix.
func commandOf(name string) string {
	for _, prefix := range []string{"parallel_driver_", "singleton_driver_", "anytime_"} {
		name = strings.TrimPrefix(name, prefix)
	}
	return name
}

// commands of the workload, by name.
var commands = map[string]func(ctx context.Context) error{
	"ready": ready,
	"sim":   func(context.Context) error { return simulate(newSDKSource()) },
	"s3_bank_engine": func(ctx context.Context) error {
		return store(ctx, sim.Bank, false)
	},
	"s3_bank_sql": func(ctx context.Context) error {
		return store(ctx, sim.Bank, true)
	},
	"s3_list_append_engine": func(ctx context.Context) error {
		return store(ctx, sim.ListAppend, false)
	},
	"s3_list_append_sql": func(ctx context.Context) error {
		return store(ctx, sim.ListAppend, true)
	},
}

func run(ctx context.Context, command string) error {
	f, ok := commands[command]
	if !ok {
		return fmt.Errorf("unknown command %q", command)
	}
	return f(ctx)
}

func newClient(ctx context.Context) (*s3.AWSClient, error) {
	bucket := os.Getenv("BUCKET")
	if bucket == "" {
		return nil, fmt.Errorf("BUCKET is not set")
	}
	return s3.NewAWSClient(ctx, s3.AWSOptions{Bucket: bucket, PathStyle: true})
}

// ready waits until the object store answers, signals that setup is complete, and idles, so that
// the container stays up for the test commands.
func ready(ctx context.Context) error {
	client, err := newClient(ctx)
	if err != nil {
		return err
	}
	for {
		_, err := client.List(ctx, "ready/")
		if err == nil {
			break
		}
		fmt.Println("waiting for the object store:", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	assert.Reachable("the workload reaches the object store at startup", nil)
	lifecycle.SetupComplete(map[string]any{"bucket": os.Getenv("BUCKET")})
	fmt.Println("setup complete")
	<-ctx.Done()
	return nil
}

// simulate runs simulator seeds with random configurations, through the engine and through SQL,
// some with disk faults and some with damaged reads from the store. A seed makes a whole run
// deterministic, so Antithesis steers the runs by choosing seeds.
func simulate(seeds rand.Source) error {
	failed := 0
	for range 8 {
		cfg := simConfig(seeds.Uint64())
		res := sim.Run(cfg)
		fmt.Println(res.Report())
		if res.Failed() {
			failed++
			fmt.Printf("seed %d failed with the configuration %+v\n", cfg.Seed, cfg)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d simulator runs found anomalies", failed)
	}
	return nil
}

// simConfig is the random configuration of the simulator run of seed.
func simConfig(seed uint64) sim.Config {
	cfg := sim.DefaultConfig(seed)
	rng := rand.New(rand.NewPCG(cfg.Seed, 0x5ea))
	cfg.Workload = sim.Workload(rng.IntN(2))
	cfg.SQL = rng.IntN(2) == 0
	cfg.Clients = 2 + rng.IntN(10)
	cfg.TxPerClient = 10 + rng.IntN(40)
	cfg.Keys = 1 + rng.IntN(6)
	cfg.Maintainers = rng.IntN(3)
	cfg.Maintenance = sim.MaintenanceMode(rng.IntN(4))
	cfg.ErrorRate = rng.Float64() * 0.3
	cfg.LostAckRate = rng.Float64() * 0.3
	cfg.CrashRate = rng.Float64() * 0.05
	cfg.OutageRate = rng.Float64() * 0.03
	cfg.MaxOutage = time.Duration(1+rng.IntN(3000)) * time.Millisecond
	cfg.OutageTimeout = time.Duration(rng.IntN(200)) * time.Millisecond
	cfg.MaxLatency = time.Duration(1+rng.IntN(50)) * time.Millisecond
	cfg.GracePeriod = time.Duration(50+rng.IntN(3000)) * time.Millisecond
	// A quarter of the runs evict from small disk caches whose files crashes damage, and a
	// quarter get reads whose bodies the store damages, independently.
	cfg.DiskFaults = rng.IntN(4) == 0
	cfg.StoreDamage = rng.IntN(4) == 0
	// A quarter of the runs each get one of the faults that make rare interleavings likely:
	// maintainers that stall between the output and the manifest write of a compaction, one
	// maintainer that compacts while the others only collect garbage, maintainers that never merge
	// the whole database, and clients partitioned right after a manifest write lands, drawn
	// independently.
	if rng.IntN(4) == 0 {
		cfg.CompactionStall = time.Duration(250+rng.IntN(751)) * time.Millisecond
	}
	cfg.OneCompactor = rng.IntN(4) == 0
	cfg.TieredCompactions = rng.IntN(4) == 0
	if rng.IntN(4) == 0 {
		cfg.CommitPartitions = 0.5 * (1 - rng.Float64())
	}
	return cfg
}

// unreachable reports whether err means that the store did not answer: a network error, a
// timeout, or a server error.
func unreachable(err error) bool {
	var netErr net.Error
	var status interface{ HTTPStatusCode() int }
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled), errors.As(err, &netErr):
		return true
	case errors.As(err, &status):
		return status.HTTPStatusCode() >= 500
	}
	return false
}

// sdkSource draws every number from the Antithesis SDK at the moment it is needed, so that
// Antithesis controls each decision of the workload. It adapts the SDK's [mathrand.Source64] to
// [rand.Source].
type sdkSource struct {
	src mathrand.Source64
}

func newSDKSource() sdkSource {
	return sdkSource{src: random.Source().(mathrand.Source64)}
}

func (s sdkSource) Uint64() uint64 {
	return s.src.Uint64()
}

// store runs a workload against the object store, in a database of its own.
func store(ctx context.Context, w sim.Workload, useSQL bool) error {
	client, err := newClient(ctx)
	if err != nil {
		return err
	}
	rng := rand.New(newSDKSource())
	var id [8]byte
	for i := range id {
		id[i] = byte(rng.Uint32())
	}
	cfg := workload.Config{
		Client:      client,
		Prefix:      "workload/" + hex.EncodeToString(id[:]),
		Workload:    w,
		SQL:         useSQL,
		Clients:     2 + rng.IntN(6),
		TxPerClient: 10 + rng.IntN(30),
		Keys:        1 + rng.IntN(5),
		MaxTimeout:  time.Duration(100+rng.IntN(5000)) * time.Millisecond,
		GracePeriod: time.Duration(1+rng.IntN(10)) * time.Second,
		// Short enough that compaction and GC run several times in a run of a few seconds.
		MaintenanceInterval: time.Duration(10+rng.IntN(190)) * time.Millisecond,
		// Small tiers, so that compaction merges segments of several tiers in a short run.
		Tiers: engine.Tiers{FanIn: 2 + rng.IntN(4), BaseBytes: int64(64 << rng.IntN(6))},
		Rand:  rng,
		Log:   func(line string) { fmt.Println(line) },
	}
	fmt.Printf("running %v (SQL %v) in %s with %d clients of %d transactions on %d keys\n", w, useSQL, cfg.Prefix, cfg.Clients, cfg.TxPerClient, cfg.Keys)
	// The run gets a few minutes; a store that stays unreachable longer is not a finding.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	res, err := workload.Run(ctx, cfg)
	assert.Sometimes(err == nil, "a run against a real object store reaches its verdict", nil)
	if err != nil {
		// Only a store that stopped answering is not a finding. Any other failure to set up or
		// read back the database is.
		var gaveUp *workload.GaveUpError
		if errors.As(err, &gaveUp) && unreachable(gaveUp.Err) {
			fmt.Println("the store was unreachable:", err)
			return nil
		}
		return err
	}
	fmt.Println(res.Report())
	if len(res.Anomalies) > 0 {
		return fmt.Errorf("%d anomalies", len(res.Anomalies))
	}
	return nil
}
