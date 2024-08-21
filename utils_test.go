package tableroll

import (
	"context"
	"os"
	"testing"
)

func tmpDir(t *testing.T) string {
	dir, err := os.MkdirTemp("", "tableroll_test")
	if err != nil {
		panic(err)
	}
	t.Cleanup(func() {
		os.RemoveAll(dir)
	})
	return dir
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}
