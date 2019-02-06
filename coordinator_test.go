package tableroll

import (
	"context"
	"io/ioutil"
	"os"
	"testing"

	"github.com/inconshreveable/log15"
)

func TestConnectOwnerCtx(t *testing.T) {
	l := log15.New()
	ctx := context.Background()
	tmpdir, err := ioutil.TempDir("", "tableroll_coord_test")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(tmpdir)
	coord1, err := lockCoordinationDir(ctx, realOS{}, l, tmpdir)
	if err != nil {
		t.Fatalf("Error getting coordination dir: %v", err)
	}
	defer coord1.Unlock()

	ctx2, cancel := context.WithCancel(ctx)
	coordErr := make(chan error)
	go func() {
		_, err := lockCoordinationDir(ctx2, realOS{}, l, tmpdir)
		coordErr <- err
	}()

	select {
	case err := <-coordErr:
		t.Fatalf("expected no coord error, should be blocked: %v", err)
	default:
	}
	cancel()
	err = <-coordErr
	if err != context.Canceled {
		t.Errorf("expected context cancel, got %v", err)
	}
}
