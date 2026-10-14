//go:build !fuse

package main

import (
	"context"
	"errors"
)

func cmdMount(context.Context, []string) error {
	return errors.New("this kudzu was built without FUSE support; rebuild it with: go build -tags fuse ./cmd/kudzu")
}
