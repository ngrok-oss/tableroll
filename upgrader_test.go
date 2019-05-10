package tableroll

import (
	"context"
	"io/ioutil"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	v1tableroll "github.com/euank/tableroll/v1_0_0"
	"github.com/inconshreveable/log15"
	"k8s.io/utils/clock"
	fakeclock "k8s.io/utils/clock/testing"
)

var l = log15.New()

func tmpDir() (string, func()) {
	dir, err := ioutil.TempDir("", "tableroll_test")
	if err != nil {
		panic(err)
	}
	return dir, func() {
		os.RemoveAll(dir)
	}
}

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
	coordDir, cleanup := tmpDir()
	defer cleanup()

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
	coordDir, cleanup := tmpDir()
	defer cleanup()

	upg1, err := newUpgrader(context.Background(), clock.RealClock{}, mockOS{pid: 1}, coordDir, WithLogger(l))
	if err != nil {
		t.Fatalf("error creating upgrader: %v", err)
	}
	if err := upg1.Ready(); err != nil {
		t.Fatalf("error marking ready: %v", err)
	}
	defer upg1.Stop()

	upgradeDone := make(chan struct{})
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
					t.Fatalf("unexpected error: %v", err)
				}
			} else {
				if fi, err = upg1.Fds.OpenFileWith(id, id, memoryOpenFile); err == nil {
					expectedFis[id] = fi
				} else if err != ErrUpgradeInProgress && err != ErrUpgradeCompleted {
					t.Fatalf("unexpected error: %v", err)
				}
			}
		}
		close(upgradeDone)
	}()

	upg2, err := newUpgrader(context.Background(), clock.RealClock{}, mockOS{pid: 2}, coordDir, WithLogger(l))
	if err != nil {
		t.Fatalf("error creating upgrader: %v", err)
	}
	if err := upg2.Ready(); err != nil {
		t.Fatalf("error marking ready: %v", err)
	}

	// we expect that upg1 should have gotten a terminal error and we should have
	// got the full set of ids it thinks it stored
	<-upgradeDone

	for id, expectedFi := range expectedFis {
		if fi, err := upg2.Fds.File(id); fi != fi || err != nil {
			t.Errorf("expected upg2 to have file for %v of %#v, but had file %#v, err %v", id, expectedFi, fi, err)
		}
	}

	upg2.Stop()
	<-upg2.UpgradeComplete()
}

// TestPIDReuse verifies that if a new server gets a pid of a previous server,
// it can still listen on the `${pid}.sock` socket correctly.
func TestPIDReuse(t *testing.T) {
	coordDir, err := ioutil.TempDir("", "tableroll_test")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(coordDir)

	server1Msgs, server2Msgs := make(chan string), make(chan string)
	server1Reqs, server2Reqs := make(chan struct{}), make(chan struct{})

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
	coordDir, cleanup := tmpDir()
	defer cleanup()

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

	syncUpgraderTimeout := make(chan struct{})
	go func() {
		// Now make an s2 that fails to ready-up
		upg2, err := newUpgrader(ctx, clock, mockOS{pid: 2}, coordDir, WithLogger(l))
		if err != nil {
			t.Fatalf("expected no error creating upgrader: %v", err)
		}
		// now the upgrader is connected, but not 'Ready', so the server should be
		// waiting on a ready timeout soon
		syncUpgraderTimeout <- struct{}{}
		// wait for the server to get a timeout before we stop, otherwise
		// `HasWaiters` could deadlock due to `Stop` causing the server to not
		// timeout
		<-syncUpgraderTimeout
		upg2.Stop()
	}()
	<-syncUpgraderTimeout
	// wait for the upgrade server to start waiting for a ready, then skip
	// forward 3 minutes since the timeout is 2 minutes by default
	for !clock.HasWaiters() {
		time.Sleep(1 * time.Millisecond)
	}
	clock.Step(3 * time.Minute)
	syncUpgraderTimeout <- struct{}{}

	server3Msgs := make(chan string)
	server3Reqs := make(chan struct{})
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
	coordDir, cleanup := tmpDir()
	defer cleanup()

	ctx1, cancel1 := context.WithTimeout(context.Background(), 1*time.Second)
	upg1, err := newUpgrader(ctx1, clock.RealClock{}, mockOS{pid: 1}, coordDir, WithLogger(l))
	if err != nil {
		t.Fatalf("error creating upgrader: %v", err)
	}
	cancel1()
	if err := upg1.Ready(); err != nil {
		t.Fatalf("unable to mark self as ready: %v", err)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	upg2, err := newUpgrader(ctx2, clock.RealClock{}, mockOS{pid: 2}, coordDir, WithLogger(l))
	if err != nil {
		t.Fatalf("error creating upgrader: %v", err)
	}
	cancel2()
	if err := upg2.Ready(); err != nil {
		t.Fatalf("unable to mark self as ready: %v", err)
	}
}

func TestUpgradeTimeout(t *testing.T) {
	ctx := context.Background()
	clock := fakeclock.NewFakeClock(time.Now())
	coordDir, cleanup := tmpDir()
	defer cleanup()

	// If upg1 times out serving the upgrade, upg2 should not be able to think it's the owner
	upg1, err := newUpgrader(ctx, clock, mockOS{pid: 1}, coordDir, WithLogger(l.New("pid", "1")), WithUpgradeTimeout(30*time.Millisecond))
	if err != nil {
		t.Fatalf("error creating upgrader: %v", err)
	}
	if err := upg1.Ready(); err != nil {
		t.Fatalf("unable to mark self as ready: %v", err)
	}

	upg2, err := newUpgrader(ctx, clock, mockOS{pid: 2}, coordDir, WithLogger(l.New("pid", "2")))
	if err != nil {
		t.Fatalf("error creating upgrader: %v", err)
	}
	// upg1 serve timeout
	clock.Step(40 * time.Millisecond)
	if err := upg2.Ready(); err == nil {
		t.Fatalf("should not be able to mark as ready after parent timed out")
	}
}

// TestUpgradeV0ToUs is basically TestUpgradeHandoff, but with the old server
// being a v1 tableroll.
func TestUpgradeV0ToUs(t *testing.T) {
	ctx := context.Background()
	coordDir, cleanup := tmpDir()
	defer cleanup()

	// Copy createTestServer into this function because the v1tableroll and
	// tableroll packages have different types, e.g. for options and upg1.Fds, so
	// the old function can't be made generic trivially by passing in a
	// constructor or such.
	upg1, err := v1tableroll.New(ctx, coordDir, v1tableroll.WithLogger(l.New("pid", "1")), v1tableroll.WithUpgradeTimeout(30*time.Millisecond))
	if err != nil {
		t.Fatalf("error creating upgrader: %v", err)
	}
	defer upg1.Stop()
	requests1 := make(chan struct{})
	responses1 := make(chan string)
	s1 := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests1 <- struct{}{}
		resp := <-responses1
		w.Write([]byte(resp))
	}))
	defer s1.Close()
	listen, err := upg1.Fds.Listen(context.Background(), "testListen", nil, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("unable to listen: %v", err)
	}
	s1.Listener = listen
	s1.Start()
	if err := upg1.Ready(); err != nil {
		t.Fatalf("unable to mark self as ready: %v", err)
	}
	c1 := s1.Client()
	c1t := c1.Transport.(closeIdleTransport)

	go func() {
		<-requests1
		responses1 <- "msg1"
	}()
	assertResp(t, s1.URL, c1, "msg1")
	// s1 is listening

	// leave a hanging client connection for s1 before upgrading, then read it after upgrading
	msg2Response := make(chan struct{})
	go func() {
		assertResp(t, s1.URL, c1, "msg2")
		msg2Response <- struct{}{}
	}()
	<-requests1

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
	responses1 <- "msg2"
	<-msg2Response
}

func assertResp(t *testing.T, url string, c *http.Client, expected string) {
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("error using test server 1: %v", err)
	}
	respData, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("error reading body: %v", err)
	}
	if expected != string(respData) {
		t.Fatalf("expected %s, got %s", expected, string(respData))
	}
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
		w.Write([]byte(resp))
	}))

	upg, err := newUpgrader(context.Background(), clock, mockOS{pid: pid}, coordDir, WithLogger(l))
	if err != nil {
		t.Fatalf("error creating upgrader: %v", err)
	}

	listen, err := upg.Fds.Listen(context.Background(), "testListen", nil, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("unable to listen: %v", err)
	}
	server.Listener = listen
	server.Start()
	if err := upg.Ready(); err != nil {
		t.Fatalf("unable to mark self as ready: %v", err)
	}
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
