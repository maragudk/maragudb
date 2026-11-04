package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
)

type statusError int

func (e statusError) Error() string       { return fmt.Sprint("status ", int(e)) }
func (e statusError) HTTPStatusCode() int { return int(e) }

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
