package tableroll

import (
	"context"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/inconshreveable/log15"
)

var l = log15.New()

func TestUpgradeHandoff(t *testing.T) {
	coordDir, err := ioutil.TempDir("", "tableroll_test")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(coordDir)

	server1Msgs, server2Msgs := make(chan string), make(chan string)
	server1Reqs, server2Reqs := make(chan struct{}), make(chan struct{})

	// Server 1 starts listening
	upg1, s1 := createTestServer(t, 1, coordDir, server1Reqs, server1Msgs)
	defer s1.Close()
	defer upg1.Stop()
	c1 := s1.Client()

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
	upg2, s2 := createTestServer(t, 2, coordDir, server2Reqs, server2Msgs)
	defer upg2.Stop()
	<-upg1.UpgradeComplete()
	s1.Listener.Close()
	defer s2.Close()
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

func createTestServer(t *testing.T, pid int, coordDir string, requests chan<- struct{}, responses <-chan string) (*Upgrader, *httptest.Server) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		l.Info("server got a request", "pid", pid)
		// Let the test harness know a client is waiting on us
		requests <- struct{}{}
		// And now respond, as requested by the test harness
		resp := <-responses
		w.Write([]byte(resp))
	}))

	upg, err := newUpgrader(context.Background(), mockOS{pid: pid}, coordDir, WithLogger(l))
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
	return upg, server
}
