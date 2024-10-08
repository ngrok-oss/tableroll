package tableroll

import (
	"context"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/inconshreveable/log15"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/clock"
	fakeclock "k8s.io/utils/clock/testing"
)

var l = log15.New()

// TestGCingUpgradeHandoff tests that the upgradehandoff test works even with
// gc running more frequently.
// This test exists because there was a point when `fd`s were owned by two file
// objects, and one file being gc'd would break everything.
func TestGCingUpgradeHandoff(t *testing.T) {
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			runtime.GC()
			time.Sleep(10 * time.Nanosecond)
		}
	}()

	for i := 0; i < 5; i++ {
		time.Sleep(10 * time.Nanosecond)
		TestUpgradeHandoff(t)
	}
	done <- struct{}{}
}

// TestUpgradeHandoff tests the happy path flow of two servers handing off the listening socket.
// It includes one client connection that spans the listener handoff and must be drained.
func TestUpgradeHandoff(t *testing.T) {
	coordDir := tmpDir(t)

	// Server 1 starts listening
	server1Reqs, server1Msgs, upg1, s1 := createTestServer(t, clock.RealClock{}, 1, coordDir)
	defer s1.Close()
	defer upg1.Stop()
	c1 := s1.Client()
	c1t := c1.Transport.(closeIdleTransport)

	go func() {
		<-server1Reqs
		server1Msgs <- "msg1"
	}()
	assertResp(t, s1.URL, c1, "msg1")
	// s1 is listening

	// leave a hanging client connection for s1 before upgrading, then read it after upgrading
	msg2Response := make(chan struct{})
	go func() {
		assertResp(t, s1.URL, c1, "msg2")
		msg2Response <- struct{}{}
	}()
	<-server1Reqs

	// now have s2 take over for s1
	server2Reqs, server2Msgs, upg2, s2 := createTestServer(t, clock.RealClock{}, 2, coordDir)
	defer upg2.Stop()
	defer s2.Close()
	<-upg1.UpgradeComplete()
	s1.Listener.Close()
	// make sure the existing tcp connections aren't re-used anymore
	c1t.CloseIdleConnections()
	go func() {
		<-server2Reqs
		server2Msgs <- "msg3"
	}()
	// Using the client for s1 should work for s2 now since the listener was passed along
	assertResp(t, s1.URL, c1, "msg3")

	// Hanging server1 request should still be service-able even after s2 has taken over
	server1Msgs <- "msg2"
	<-msg2Response
}

func TestMutableUpgrading(t *testing.T) {
	coordDir := tmpDir(t)

	upg1, err := newUpgrader(context.Background(), clock.RealClock{}, coordDir, "1", WithLogger(l))
	require.NoError(t, err)
	require.NoError(t, upg1.Ready())
	defer upg1.Stop()

	upgradeDone := make(chan error, 1)
	expectedFis := map[string]*os.File{}
	// Mutably add a bunch of fds to the store at random, make sure that all the ones that were added without error are inherited
	go func() {
		var err error
		var fi *os.File
		for err != ErrUpgradeCompleted {
			// add 2/3 of the time, remove 1/3 of the time, pool of 1000 ids
			id := strconv.Itoa(rand.Intn(1000))
			if expectedFis[id] != nil {
				if err = upg1.Fds.Remove(id); err == nil {
					delete(expectedFis, id)
				} else if err != ErrUpgradeInProgress && err != ErrUpgradeCompleted {
					upgradeDone <- err
					return
				}
			} else {
				if fi, err = upg1.Fds.OpenFileWith(id, id, memoryOpenFile); err == nil {
					expectedFis[id] = fi
				} else if err != ErrUpgradeInProgress && err != ErrUpgradeCompleted {
					upgradeDone <- err
					return
				}
			}
		}
		close(upgradeDone)
	}()

	upg2, err := newUpgrader(context.Background(), clock.RealClock{}, coordDir, "2", WithLogger(l))
	require.NoError(t, err)
	require.NoError(t, upg2.Ready())

	// we expect that upg1 should have gotten a terminal error and we should have
	// got the full set of ids it thinks it stored
	require.NoError(t, <-upgradeDone)

	for id, expectedFi := range expectedFis {
		if fi, err := upg2.Fds.File(id); err != nil {
			t.Errorf("expected upg2 to have file for %v of %#v, but had file %#v, err %v", id, expectedFi, fi, err)
		}
	}

	upg2.Stop()
	<-upg2.UpgradeComplete()
}

// TestPIDReuse verifies that if a new server gets a pid of a previous server,
// it can still listen on the `${pid}.sock` socket correctly.
func TestPIDReuse(t *testing.T) {
	coordDir := tmpDir(t)

	// Server 1 starts listening
	server1Reqs, server1Msgs, upg1, s1 := createTestServer(t, clock.RealClock{}, 1, coordDir)
	defer s1.Close()
	defer upg1.Stop()
	c1 := s1.Client()
	c1t := c1.Transport.(closeIdleTransport)

	go func() {
		<-server1Reqs
		server1Msgs <- "msg1"
	}()
	assertResp(t, s1.URL, c1, "msg1")
	// s1 is listening

	// now have s2 take over for s1
	server2Reqs, server2Msgs, upg2, s2 := createTestServer(t, clock.RealClock{}, 2, coordDir)
	defer upg2.Stop()
	defer s2.Close()
	<-upg1.UpgradeComplete()
	// Shut down server 1, have a new server reuse it
	s1.Listener.Close()
	c1t.CloseIdleConnections()
	upg1.Stop()

	go func() {
		<-server2Reqs
		server2Msgs <- "msg2"
	}()
	// Using the client for s1 should work for s2 now since the listener was passed along
	assertResp(t, s1.URL, c1, "msg2")

	// server 3, reusing pid1
	server1Reqs, server1Msgs, upg3, s3 := createTestServer(t, clock.RealClock{}, 1, coordDir)
	defer upg3.Stop()
	defer s3.Close()

	<-upg2.UpgradeComplete()
	s2.Close()

	go func() {
		<-server1Reqs
		server1Msgs <- "msg3"
	}()
	assertResp(t, s1.URL, c1, "msg3")
}

// TestFdPassMultipleTimes tests that a given owner process can attempt to pass
// the same listening fds to multiple processes if a 'Ready' is not received in
// time.
func TestFdPassMultipleTimes(t *testing.T) {
	ctx := context.Background()
	clock := fakeclock.NewFakeClock(time.Now())
	coordDir := tmpDir(t)

	server1Reqs, server1Msgs, upg1, s1 := createTestServer(t, clock, 1, coordDir)
	defer upg1.Stop()
	defer s1.Close()
	c1 := s1.Client()
	c1t := c1.Transport.(closeIdleTransport)

	go func() {
		<-server1Reqs
		server1Msgs <- "msg1"
	}()
	assertResp(t, s1.URL, c1, "msg1")
	// s1 listening

	syncUpgraderTimeout := make(chan error, 1)
	go func() {
		// Now make an s2 that fails to ready-up
		upg2, err := newUpgrader(ctx, clock, coordDir, "2", WithLogger(l))
		if err != nil {
			syncUpgraderTimeout <- err
			return
		}
		// now the upgrader is connected, but not 'Ready', so the server should be
		// waiting on a ready timeout soon
		syncUpgraderTimeout <- nil
		// wait for the server to get a timeout before we stop, otherwise
		// `HasWaiters` could deadlock due to `Stop` causing the server to not
		// timeout
		<-syncUpgraderTimeout
		upg2.Stop()
	}()
	require.NoError(t, <-syncUpgraderTimeout)
	// wait for the upgrade server to start waiting for a ready, then skip
	// forward 3 minutes since the timeout is 2 minutes by default
	for !clock.HasWaiters() {
		time.Sleep(1 * time.Millisecond)
	}
	clock.Step(3 * time.Minute)
	close(syncUpgraderTimeout)

	// now see that we get a working s3
	server3Reqs, server3Msgs, upg3, s3 := createTestServer(t, clock, 3, coordDir)
	defer upg3.Stop()
	defer s3.Close()
	<-upg1.UpgradeComplete()
	s1.Listener.Close()
	c1t.CloseIdleConnections()

	go func() {
		<-server3Reqs
		server3Msgs <- "msg3"
	}()
	assertResp(t, s1.URL, c1, "msg3")
}

// TestUpgradeHandoffCloseCtx closes the 'New' context as soon as possible and
// verifies that the context was only for instantiation.
func TestUpgradeHandoffCloseCtx(t *testing.T) {
	coordDir := tmpDir(t)

	ctx1, cancel1 := context.WithTimeout(context.Background(), 1*time.Second)
	upg1, err := newUpgrader(ctx1, clock.RealClock{}, coordDir, "1", WithLogger(l))
	require.NoError(t, err)
	defer upg1.Stop()
	cancel1()
	require.NoError(t, upg1.Ready())

	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	upg2, err := newUpgrader(ctx2, clock.RealClock{}, coordDir, "2", WithLogger(l))
	require.NoError(t, err)
	defer upg2.Stop()
	cancel2()
	require.NoError(t, upg2.Ready())
}

func TestUpgradeTimeout(t *testing.T) {
	ctx := context.Background()
	clock := fakeclock.NewFakeClock(time.Now())
	coordDir := tmpDir(t)

	// If upg1 times out serving the upgrade, upg2 should not be able to think it's the owner
	upg1, err := newUpgrader(ctx, clock, coordDir, "1", WithLogger(l.New("pid", "1")), WithUpgradeTimeout(30*time.Millisecond))
	require.NoError(t, err)
	require.NoError(t, upg1.Ready())

	upg2, err := newUpgrader(ctx, clock, coordDir, "2", WithLogger(l.New("pid", "2")))
	require.NoError(t, err)
	// upg1 serve timeout
	for !clock.HasWaiters() {
		time.Sleep(1 * time.Millisecond)
	}
	clock.Step(40 * time.Millisecond)
	// Hack: we need to wait for upg2 to actually close the connection/file as
	// part of the timeout, so wait a sec to make sure they're closed...
	// A more proper fix would be to let us instrument the upgrader with a
	// callback or upgrade failure channel so we can explicitly wait for the timeout here.
	time.Sleep(10 * time.Millisecond)
	if err := upg2.Ready(); err == nil {
		t.Fatalf("should not be able to mark as ready after parent timed out")
	}
}

// TestFTestFailedUpgradeAccept tests that 'ln.Accept' works for a listener
// correctly after a failed upgrade. This is a regression test for a bug that
// left file descriptors in 'blocking' mode, which resulted in accept + close
// deadlocking.
func TestFailedUpgradeListen(t *testing.T) {
	ctx := context.Background()
	coordDir := tmpDir(t)

	upg1, err := newUpgrader(ctx, clock.RealClock{}, coordDir, "1", WithLogger(l.New("pid", "1")))
	require.Nil(t, err)
	ln, err := upg1.Fds.Listen(ctx, "id", &net.ListenConfig{}, "tcp", "127.0.0.1:0")
	require.Nil(t, err)
	require.NoError(t, upg1.Ready())

	// fail an upgrade
	upg2, err := newUpgrader(ctx, clock.RealClock{}, coordDir, "2", WithLogger(l.New("pid", "2")))
	require.Nil(t, err)
	upg2.Stop()

	// Accept, then Close
	go func() {
		_, err := ln.Accept()
		require.Contains(t, err.Error(), "use of closed network connection")
	}()
	// let the accept happen first
	time.Sleep(1 * time.Millisecond)
	err = ln.Close()
	require.Nil(t, err)
	// if we aren't deadlocked here, the regression test passes
}

func assertResp(t *testing.T, url string, c *http.Client, expected string) {
	resp, err := c.Get(url)
	require.NoError(t, err)
	respData, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, expected, string(respData))
}

func createTestServer(t *testing.T, clock clock.Clock, pid int, coordDir string) (chan struct{}, chan string, *Upgrader, *httptest.Server) {
	requests := make(chan struct{})
	responses := make(chan string)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		l.Info("server got a request", "pid", pid)
		// Let the test harness know a client is waiting on us
		requests <- struct{}{}
		// And now respond, as requested by the test harness
		resp := <-responses
		_, err := w.Write([]byte(resp))
		if err != nil {
			panic(err)
		}
	}))

	upg, err := newUpgrader(context.Background(), clock, coordDir, strconv.Itoa(pid), WithLogger(l))
	require.NoError(t, err)

	listen, err := upg.Fds.Listen(context.Background(), "testListen", nil, "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server.Listener = listen
	server.Start()
	require.NoError(t, upg.Ready())
	return requests, responses, upg, server
}

func memoryOpenFile(name string) (*os.File, error) {
	_, w, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	return w, nil
}

type closeIdleTransport interface {
	CloseIdleConnections()
}
