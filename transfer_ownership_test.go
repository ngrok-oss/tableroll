package tableroll

import (
	"context"
	"io/ioutil"
	"os"
	"testing"

	"github.com/inconshreveable/log15"
	"github.com/pkg/errors"
	"k8s.io/utils/clock"
)

func TestGetFilesCtxCancel(t *testing.T) {
	ctx := context.Background()
	l := log15.New()
	tmpdir, err := ioutil.TempDir("", "tableroll_getfiles")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(tmpdir)
	parent := newCoordinator(clock.RealClock{}, mockOS{pid: 1}, l, tmpdir)
	parent.Listen(ctx)
	parent.Lock(ctx)
	parent.BecomeOwner()
	parent.Unlock()

	newParent := newCoordinator(clock.RealClock{}, mockOS{pid: 2}, l, tmpdir)

	sess, err := connectToCurrentOwner(ctx, l, newParent)
	if err != nil {
		t.Fatalf("could not connect to parent: %v", err)
	}

	ctx2, cancel := context.WithCancel(ctx)
	getFilesErr := make(chan error)
	go func() {
		_, err := sess.getFiles(ctx2)
		getFilesErr <- err
	}()

	select {
	case err := <-getFilesErr:
		t.Fatalf("expected no error yet: %v", err)
	default:
	}
	cancel()
	err = <-getFilesErr
	if errors.Cause(err) != context.Canceled {
		t.Fatalf("expected cancelled error, got: %v", err)
	}
}
