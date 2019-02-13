package tableroll

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
)

const (
	MsgReady         = "ready"
	MsgServedRequest = "served-http-request"
)

const testAddr = "127.0.0.1:44090"

func TestBasicProcessUpgrade(t *testing.T) {
	tmpdir, cleanup := tmpDir()
	defer cleanup()

	stdout, errC, exitC, cmd1 := runHelper(t, tmpdir, "main1")
	defer func() {
		if cmd1.Process != nil {
			cmd1.Process.Kill()
		}
	}()

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
		t.Fatalf("expected no error in get")
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
	stdout2, errC2, exitC2, cmd2 := runHelper(t, tmpdir, "main1")
	defer func() {
		if cmd2.Process != nil {
			cmd2.Process.Kill()
		}
	}()

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

func runHelper(t *testing.T, dir string, funcName string) (<-chan string, <-chan error, <-chan int, *exec.Cmd) {
	child := exec.Command(os.Args[0], "-test.run=TestSpawnHelper", "--")
	stderr, _ := child.StderrPipe()
	stdout, _ := child.StdoutPipe()

	var stderrBuffer bytes.Buffer
	go io.Copy(&stderrBuffer, stderr)

	child.Env = append(child.Env, []string{
		"MAIN_FUNC=" + funcName,
		"__TABLEROLL_TEST_PROCESS=1",
		"TABLEROLL_DIR=" + dir,
	}...)

	stdoutChan := make(chan string, 1)
	errorChan := make(chan error, 1)
	exitChan := make(chan int, 1)

	stdoutScanner := bufio.NewScanner(stdout)
	go func() {
		for stdoutScanner.Scan() {
			text := strings.TrimSpace(stdoutScanner.Text())
			if text != "" {
				stdoutChan <- text
			}
		}
	}()
	go func() {
		err := child.Run()
		if stderrBuffer.Len() != 0 {
			errorChan <- fmt.Errorf("stderr: %v", stderrBuffer.String())
		}
		if err != nil {
			errorChan <- err
		}
		exitChan <- 0
	}()

	return stdoutChan, errorChan, exitChan, child
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
	default:
		fmt.Fprintf(os.Stderr, "unknown main function: %v", funcName)
		os.Exit(1)
	}
}

func main1() int {
	ctx := context.Background()
	tableRollDir := os.Getenv("TABLEROLL_DIR")

	upg, err := New(ctx, tableRollDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v", err)
		return 1
	}
	ln, err := upg.Fds.Listen(ctx, "127.0.0.1:44090", nil, "tcp", "127.0.0.1:44090")
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
