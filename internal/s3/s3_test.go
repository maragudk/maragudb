package s3_test

import (
	"testing"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/s3"
)

func TestCounts_Add(t *testing.T) {
	t.Run("adds every kind of request, and Sub undoes it", func(t *testing.T) {
		a := s3.Counts{Get: 1, GetConditional: 2, Put: 3, PutConditional: 4, Delete: 5, List: 6}
		b := s3.Counts{Get: 10, GetConditional: 20, Put: 30, PutConditional: 40, Delete: 50, List: 60}
		is.Equal(t, s3.Counts{Get: 11, GetConditional: 22, Put: 33, PutConditional: 44, Delete: 55, List: 66}, a.Add(b))
		is.Equal(t, a, a.Add(b).Sub(b))
	})
}
