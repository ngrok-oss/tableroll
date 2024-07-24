package tableroll

import (
	"context"
	"io"
	"testing"

	"github.com/inconshreveable/log15"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/clock"
)

// TestConnectOwner is a happy-path test of using the coordinator
func TestConnectOwner(t *testing.T) {
	l := log15.New()
	ctx := context.Background()
	tmpdir := tmpDir(t)

	coord1 := newCoordinator(clock.RealClock{}, l, tmpdir, "1")
	coord2 := newCoordinator(clock.RealClock{}, l, tmpdir, "2")

	coord1l, err := coord1.Listen(ctx)
	require.NoError(t, err)
	require.NoError(t, coord1.Lock(ctx))
	require.NoError(t, coord1.BecomeOwner())
	require.NoError(t, coord1.Unlock())

	connw, err := coord2.ConnectOwner(ctx)
	require.NoError(t, err)

	connErr := make(chan error, 1)
	go func() {
		defer close(connErr)
		if _, err := connw.Write([]byte("hello world")); err != nil {
			connErr <- err
		}
		if err := connw.Close(); err != nil {
			connErr <- err
		}
	}()

	connr, err := coord1l.Accept()
	if err != nil {
		t.Fatalf("acccept err: %v", err)
	}
	data, err := io.ReadAll(connr)
	require.NoError(t, err)
	require.Equal(t, "hello world", string(data))
	require.NoError(t, <-connErr)
}

// TestLockCoordinationDirCtxCancel tests that a call to `lockCoordinationDir` can be
// canceled by canceling the passed in context.
func TestLockCoordinationDirCtxCancel(t *testing.T) {
	l := log15.New()
	ctx := testCtx(t)
	tmpdir := tmpDir(t)
	coord1 := newCoordinator(clock.RealClock{}, l, tmpdir, "1")
	coord2 := newCoordinator(clock.RealClock{}, l, tmpdir, "2")
	require.NoError(t, coord1.Lock(ctx))
	defer func() { require.NoError(t, coord1.Unlock()) }()

	ctx2, cancel := context.WithCancel(ctx)
	coordErr := make(chan error)
	go func() {
		err := coord2.Lock(ctx2)
		coordErr <- err
	}()

	select {
	case err := <-coordErr:
		t.Fatalf("expected no coord error, should be blocked: %v", err)
	default:
	}
	cancel()
	err := <-coordErr
	if err != context.Canceled {
		t.Errorf("expected context cancel, got %v", err)
	}
}
