package tableroll

import (
	"context"
	"io/ioutil"
	"os"
	"testing"

	"github.com/inconshreveable/log15"
	"k8s.io/utils/clock"
)

// TestConnectOwner is a happy-path test of using the coordinator
func TestConnectOwner(t *testing.T) {
	l := log15.New()
	ctx := context.Background()
	tmpdir, err := ioutil.TempDir("", "tableroll_coord_test")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(tmpdir)

	coord1 := newCoordinator(clock.RealClock{}, mockOS{pid: 1}, l, tmpdir)
	coord2 := newCoordinator(clock.RealClock{}, mockOS{pid: 2}, l, tmpdir)

	coord1l, err := coord1.Listen(ctx)
	if err != nil {
		t.Fatalf("Unable to listen: %v", err)
	}
	coord1.Lock(ctx)
	coord1.BecomeOwner()
	coord1.Unlock()

	connw, err := coord2.ConnectOwner(ctx)
	if err != nil {
		t.Fatalf("unable to connect to owner")
	}

	go func() {
		connw.Write([]byte("hello world"))
		connw.Close()
	}()

	connr, err := coord1l.Accept()
	if err != nil {
		t.Fatalf("acccept err: %v", err)
	}
	data, err := ioutil.ReadAll(connr)
	if err != nil {
		t.Fatalf("read err: %v", err)
	}
	if string(data) != "hello world" {
		t.Fatalf("expected to read %q; got %q", "hello world", string(data))
	}
}

// TestLockCoordinationDirCtxCancel tests that a call to `lockCoordinationDir` can be
// canceled by canceling the passed in context.
func TestLockCoordinationDirCtxCancel(t *testing.T) {
	l := log15.New()
	ctx := context.Background()
	tmpdir, err := ioutil.TempDir("", "tableroll_coord_test")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(tmpdir)
	coord1 := newCoordinator(clock.RealClock{}, mockOS{pid: 1}, l, tmpdir)
	coord2 := newCoordinator(clock.RealClock{}, mockOS{pid: 2}, l, tmpdir)
	err = coord1.Lock(ctx)
	if err != nil {
		t.Fatalf("Error getting coordination dir: %v", err)
	}
	defer coord1.Unlock()

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
	err = <-coordErr
	if err != context.Canceled {
		t.Errorf("expected context cancel, got %v", err)
	}
}
