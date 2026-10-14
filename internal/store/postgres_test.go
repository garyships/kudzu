package store_test

import (
	"os"
	"testing"

	"github.com/garyships/kudzu/internal/store"
	"github.com/garyships/kudzu/internal/store/storetest"
)

// TestPostgresMeta runs when KUDZU_TEST_POSTGRES holds a database URL.
func TestPostgresMeta(t *testing.T) {
	dsn := os.Getenv("KUDZU_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("KUDZU_TEST_POSTGRES not set")
	}
	store.TestMeta(t, storetest.Postgres(t, dsn))
}
