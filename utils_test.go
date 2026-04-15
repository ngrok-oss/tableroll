package tableroll

import (
	"os"
	"testing"
)

func tmpDir(t *testing.T) string {
	dir, err := os.MkdirTemp("", "tableroll_test")
	if err != nil {
		panic(err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(dir)
	})
	return dir
}
