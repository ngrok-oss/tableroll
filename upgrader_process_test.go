package tableroll

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	MsgReady         = "ready"
	MsgServedRequest = "served-request"
)

// loopbackAddr finds a free ephemeral loopback address and returns it
func loopbackTCPAddr(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not listen: %v", err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func TestBasicProcessUpgrade(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tmpdir, cleanup := tmpDir()
	defer cleanup()

	testAddr := loopbackTCPAddr(t)
	t.Logf("running with addr %v", testAddr)

	stdout, errC, exitC := runHelper(t, ctx, tmpdir, "main1", testAddr)

	select {
	case msg := <-stdout:
		if msg != MsgReady {
			t.Fatalf("expected ready, got %q", msg)
		}
	case err := <-errC:
		t.Fatalf("unexpected err: %v", err)
	case exit := <-exitC:
		t.Fatalf("unexpected exit: %v", exit)
	}

	// process 1 listening and ready, make a request
	client := &http.Client{
		Transport: &http.Transport{
			DisableKeepAlives: true,
		},
	}
	_, err := client.Get("http://" + testAddr)
	if err != nil {
		t.Fatalf("expected no error in get: %v", err)
	}
	// server1 should have got the request
	select {
	case msg := <-stdout:
		if msg != MsgServedRequest {
			t.Fatalf("expected served request, got %q", msg)
		}
	case err := <-errC:
		t.Fatalf("unexpected err: %v", err)
	case exit := <-exitC:
		t.Fatalf("unexpected exit: %v", exit)
	}

	// Now pass fds to process 2
	stdout2, errC2, exitC2 := runHelper(t, ctx, tmpdir, "main1", testAddr)

	// process 1 should exit
	select {
	case msg := <-stdout:
		t.Fatalf("expected exit, got stdout %v", msg)
	case err := <-errC:
		t.Fatalf("unexpected err: %v", err)
	case exit := <-exitC:
		if exit != 0 {
			t.Fatalf("expected 0 exit: %v", exit)
		}
	}

	// process 2 should be ready
	select {
	case msg := <-stdout2:
		if msg != MsgReady {
			t.Fatalf("expected ready, got %v", msg)
		}
	case err := <-errC2:
		t.Fatalf("unexpected err: %v", err)
	case exit := <-exitC2:
		t.Fatalf("unexpected exit: %v", exit)
	}

	// Process 2 will now serve our request
	_, err = client.Get("http://" + testAddr)
	if err != nil {
		t.Fatalf("expected no error in get")
	}
	// server2 should have got the request
	select {
	case msg := <-stdout2:
		if msg != MsgServedRequest {
			t.Fatalf("expected served request, got %q", msg)
		}
	case err := <-errC2:
		t.Fatalf("unexpected err: %v", err)
	case exit := <-exitC2:
		t.Fatalf("unexpected exit: %v", exit)
	}
}

func TestUnixMultiProcessUpgrade(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tmpdir, cleanup := tmpDir()
	defer cleanup()

	sock := filepath.Join(tmpdir, "testsock")
	stdout, errC, exitC := runHelper(t, ctx, tmpdir, "main2", "")

	select {
	case msg := <-stdout:
		if msg != MsgReady {
			t.Fatalf("expected ready, got %q", msg)
		}
	case err := <-errC:
		t.Fatalf("unexpected err: %v", err)
	case exit := <-exitC:
		t.Fatalf("unexpected exit: %v", exit)
	}

	// process 1 listening and ready, make a request
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("expected no error in get")
	}
	data, _ := ioutil.ReadAll(conn)
	if string(data) != "hello world" {
		t.Fatalf("expected hello world, got %s", data)
	}
	// server1 should have got the request
	select {
	case msg := <-stdout:
		if msg != MsgServedRequest {
			t.Fatalf("expected served request, got %q", msg)
		}
	case err := <-errC:
		t.Fatalf("unexpected err: %v", err)
	case exit := <-exitC:
		t.Fatalf("unexpected exit: %v", exit)
	}

	prevExit := exitC
	// now pass fds through 10 more processes
	for i := 0; i < 10; i++ {
		// Now pass fds to process n
		stdoutn, errCn, exitCn := runHelper(t, ctx, tmpdir, "main2", "")

		// process n-1 should exit
		exit := <-prevExit
		if exit != 0 {
			t.Fatalf("expected 0 exit: %v", exit)
		}

		// process 2 should be ready
		select {
		case msg := <-stdoutn:
			if msg != MsgReady {
				t.Fatalf("expected ready, got %v", msg)
			}
		case err := <-errCn:
			t.Fatalf("unexpected err: %v", err)
		case exit := <-exitCn:
			t.Fatalf("unexpected exit: %v", exit)
		}

		// Process 2 will now serve our request
		conn, err = net.Dial("unix", sock)
		if err != nil {
			t.Fatalf("expected no error in get")
		}
		data, _ = ioutil.ReadAll(conn)
		if string(data) != "hello world" {
			t.Fatalf("expected hello world, got %s", data)
		}
		// server1 should have got the request
		select {
		case msg := <-stdoutn:
			if msg != MsgServedRequest {
				t.Fatalf("expected served request, got %q", msg)
			}
		case err := <-errCn:
			t.Fatalf("unexpected err: %v", err)
		case exit := <-exitCn:
			t.Fatalf("unexpected exit: %v", exit)
		}

		prevExit = exitCn
	}
}

func TestMaxSocketUpg(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tmpdir, cleanup := tmpDir()
	defer cleanup()

	stdout, errC, exitC1 := runHelper(t, ctx, tmpdir, "maxSocketOpener", "")
	select {
	case msg := <-stdout:
		if msg != MsgReady {
			t.Fatalf("expected ready, got %q", msg)
		}
	case err := <-errC:
		t.Fatalf("unexpected err: %v", err)
	case exit := <-exitC1:
		t.Fatalf("unexpected exit: %v", exit)
	}

	stdout, errC, exitC := runHelper(t, ctx, tmpdir, "maxSocketOpener", "")
	select {
	case msg := <-stdout:
		if msg != MsgReady {
			t.Fatalf("expected ready, got %q", msg)
		}
	case err := <-errC:
		t.Fatalf("unexpected err: %v", err)
	case exit := <-exitC:
		t.Fatalf("unexpected exit: %v", exit)
	}

	cancel()
	<-exitC1
	<-exitC
}

// runHelper runs one of several "main" functions built into this testing
// binary.
// These are used to allow integration style testing of multiple processes.
// This function takes a tableroll directory and function name to execute, and
// optionally a tcp address to listen on which is used by certain tests.
func runHelper(t *testing.T, ctx context.Context, dir string, funcName string, addr string) (<-chan string, <-chan error, <-chan int) {
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=TestSpawnHelper", "--")
	stderr, _ := child.StderrPipe()
	stdout, _ := child.StdoutPipe()

	var stderrBuffer bytes.Buffer
	stderrEOF := make(chan struct{})
	go func() {
		io.Copy(&stderrBuffer, stderr)
		stderrEOF <- struct{}{}
	}()

	child.Env = append(child.Env, []string{
		"MAIN_FUNC=" + funcName,
		"__TABLEROLL_TEST_PROCESS=1",
		"TABLEROLL_DIR=" + dir,
		"LISTEN_ADDR=" + addr,
	}...)

	stdoutChan := make(chan string, 1)
	errorChan := make(chan error, 1)
	exitChan := make(chan int, 1)

	stdoutScanner := bufio.NewScanner(stdout)
	go func() {
		for stdoutScanner.Scan() {
			text := strings.TrimSpace(stdoutScanner.Text())
			if text != "" && !strings.HasPrefix(text, "--- FAIL") {
				// avoid sending '-test.run' metadata too
				stdoutChan <- text
			}
		}
	}()
	go func() {
		err := child.Run()
		<-stderrEOF
		if stderrBuffer.Len() != 0 {
			errorChan <- fmt.Errorf("stderr: %v", stderrBuffer.String())
		}
		if err != nil {
			errorChan <- err
		}
		exitChan <- 0
		close(exitChan)
	}()

	return stdoutChan, errorChan, exitChan
}

// TestSpawnUpgrader isn't a real test, it's run from other tests in this file
// in order to have a real child process to play with.
func TestSpawnHelper(t *testing.T) {
	if os.Getenv("__TABLEROLL_TEST_PROCESS") == "" {
		// running as a 'go test' test, nothing to do here
		return
	}

	funcName := os.Getenv("MAIN_FUNC")
	switch funcName {
	case "main1":
		os.Exit(main1())
	case "main2":
		os.Exit(main2())
	case "maxSocketOpener":
		os.Exit(maxSocketOpener())
	default:
		fmt.Fprintf(os.Stderr, "unknown main function: %v", funcName)
		os.Exit(1)
	}
}

func main1() int {
	ctx := context.Background()
	tableRollDir := os.Getenv("TABLEROLL_DIR")
	listenAddr := os.Getenv("LISTEN_ADDR")

	upg, err := New(ctx, tableRollDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v", err)
		return 1
	}
	ln, err := upg.Fds.Listen(ctx, listenAddr, nil, "tcp", listenAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v", err)
		return 1
	}
	server := &http.Server{
		Handler: http.HandlerFunc(func(r http.ResponseWriter, req *http.Request) {
			fmt.Println(MsgServedRequest)
			r.Write([]byte(fmt.Sprintf("hello from %v!\n", os.Getpid())))
		}),
	}
	go server.Serve(ln)

	if err := upg.Ready(); err != nil {
		fmt.Fprintf(os.Stderr, "%v", err)
		return 1
	}
	fmt.Println(MsgReady)

	<-upg.UpgradeComplete()
	_ = server.Shutdown(ctx)
	return 0
}

// main1, but unix sockets
func main2() int {
	ctx := context.Background()
	tableRollDir := os.Getenv("TABLEROLL_DIR")

	upg, err := New(ctx, tableRollDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v", err)
		return 1
	}

	sockPath := filepath.Join(tableRollDir, "testsock")
	ln, err := upg.Fds.Listen(ctx, sockPath, nil, "unix", sockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v", err)
		return 1
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Write([]byte("hello world"))
			conn.Close()
			fmt.Println(MsgServedRequest)
		}
	}()

	if err := upg.Ready(); err != nil {
		fmt.Fprintf(os.Stderr, "%v", err)
		return 1
	}
	fmt.Println(MsgReady)

	<-upg.UpgradeComplete()
	_ = ln.Close()
	return 0
}

func maxSocketOpener() int {
	ctx := context.Background()
	tableRollDir := os.Getenv("TABLEROLL_DIR")

	upg, err := New(ctx, tableRollDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v", err)
		return 1
	}

	if err := upg.Ready(); err != nil {
		fmt.Fprintf(os.Stderr, "%v", err)
		return 1
	}

	// Assume we can always open at least 500, ulimits are usually around 1024
	// and 500 is enough to catch the regression this is testing for.
	minFds := 500
	lns := []net.Listener{}
	var i int
	for i = 0; err == nil; i++ {
		ln, err := upg.Fds.ListenWith(fmt.Sprintf("ln-%v", i), "tcp", "127.0.0.1:0", net.Listen)
		if err != nil {
			break
		}
		lns = append(lns, ln)
	}
	if i < minFds {
		fmt.Fprintf(os.Stderr, "could not open %v fds, only opened %v: %v", minFds, i+1, err)
		return 1
	}

	// Close the last 10 to free up enough fds for the upgrade to happen
	lns, toClose := lns[0:len(lns)-10], lns[len(lns)-10:]
	for _, ln := range toClose {
		ln.Close()
	}

	fmt.Println(MsgReady)
	<-upg.UpgradeComplete()
	for _, ln := range lns {
		ln.Close()
	}
	return 0
}
