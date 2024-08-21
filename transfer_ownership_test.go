package tableroll

import (
	"context"
	"testing"

	"github.com/inconshreveable/log15"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/clock"
)

func TestGetFilesCtxCancel(t *testing.T) {
	ctx := context.Background()
	l := log15.New()
	tmpdir := tmpDir(t)
	parent := newCoordinator(clock.RealClock{}, l, tmpdir, "1")
	_, err := parent.Listen(ctx)
	require.NoError(t, err)
	require.NoError(t, parent.Lock(ctx))
	require.NoError(t, parent.BecomeOwner())
	require.NoError(t, parent.Unlock())

	newParent := newCoordinator(clock.RealClock{}, l, tmpdir, "2")

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
