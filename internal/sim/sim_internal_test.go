package sim

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/s3"
	"maragu.dev/maragudb/internal/s3fake"
)

func TestClient_checkError(t *testing.T) {
	newClient := func(t *testing.T, useSQL bool) (*client, *s3fake.Client) {
		t.Helper()
		damage := false
		fake := s3fake.New(s3fake.WithFaults(s3fake.Faults{Damage: func(key string, body []byte) []byte {
			if damage {
				body[0] ^= 1
			}
			return body
		}}))
		_, err := fake.Put(t.Context(), "k", []byte("v"), s3.PutOptions{})
		is.NotError(t, err)
		c := &client{run: &run{cfg: Config{SQL: useSQL}, fake: fake}, proc: &s3fake.Process{Name: "client0"}}
		c.damagedAt = fake.Damaged("client0")
		damage = true
		return c, fake
	}
	read := func(t *testing.T, c *client, fake *s3fake.Client) {
		t.Helper()
		_, err := fake.Get(s3fake.WithProcess(context.WithoutCancel(t.Context()), c.proc), "k", s3.GetOptions{})
		is.NotError(t, err)
	}
	corrupt := fmt.Errorf("%w: a block fails its checksum", engine.ErrCorrupt)

	t.Run("finds an ErrCorrupt of an operation none of whose reads the store damaged", func(t *testing.T) {
		c, _ := newClient(t, false)
		c.checkError(corrupt)
		is.Equal(t, 1, len(c.run.anoms))
		var a *Anomaly
		is.True(t, errors.As(c.run.anoms[0], &a))
		is.Equal(t, CheckerDamage, a.Checker)
	})

	t.Run("accepts an ErrCorrupt of an operation one of whose reads the store damaged", func(t *testing.T) {
		c, fake := newClient(t, true)
		read(t, c, fake)
		c.checkError(corrupt)
		is.Equal(t, 0, len(c.run.anoms))
	})

	t.Run("finds an error of no kind that the engine or the store gives through the engine, but not through SQL", func(t *testing.T) {
		c, fake := newClient(t, false)
		read(t, c, fake)
		c.checkError(errors.New("engine: reading key: unexpected EOF"))
		is.Equal(t, 1, len(c.run.anoms))
		for _, err := range []error{fmt.Errorf("x: %w", engine.ErrConflict), engine.ErrCommitUnknown, engine.ErrSnapshotExpired, s3fake.ErrInjected, s3fake.ErrUnavailable, s3fake.ErrCrashed, nil} {
			c.checkError(err)
		}
		is.Equal(t, 1, len(c.run.anoms))
		c, _ = newClient(t, true)
		c.checkError(errors.New("exec: duplicate key"))
		is.Equal(t, 0, len(c.run.anoms))
	})
}
