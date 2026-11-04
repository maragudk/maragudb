package tigristest_test

import (
	"testing"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/tigristest"
	"maragu.dev/maragudb/internal/verify"
)

func TestMain(m *testing.M) {
	verify.Main(m)
}

func TestMissing(t *testing.T) {
	t.Run("names every unset required variable and not the optional region", func(t *testing.T) {
		verify.Covers(t, "the Tigris suite's check names every unset required variable, which the suite reports with t.Fatalf instead of skipping", "AC-7.2")
		env := map[string]string{"AWS_ACCESS_KEY_ID": "x", "BUCKET": "b"}
		missing := tigristest.Missing(func(k string) string { return env[k] })
		is.EqualSlice(t, []string{"AWS_SECRET_ACCESS_KEY", "AWS_ENDPOINT_URL_S3"}, missing)
	})

	t.Run("is empty when everything is set", func(t *testing.T) {
		env := map[string]string{"AWS_ACCESS_KEY_ID": "x", "AWS_SECRET_ACCESS_KEY": "y", "AWS_ENDPOINT_URL_S3": "https://e", "BUCKET": "b"}
		is.Equal(t, 0, len(tigristest.Missing(func(k string) string { return env[k] })))
	})
}
