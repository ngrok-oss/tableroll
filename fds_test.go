package tableroll

import (
	"context"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestFdsListen(t *testing.T) {
	ctx := context.Background()
	addrs := [][2]string{
		{"unix", ""},
		{"tcp", "localhost:0"},
	}

	fds := newFds(l, nil)

	for _, addr := range addrs {
		ln, err := fds.Listen(ctx, "1", nil, addr[0], addr[1])
		if err != nil {
			t.Fatal(err)
		}
		if ln == nil {
			t.Fatal("Missing listener", addr)
		}
		ln.Close()
	}
}

func TestFdsListener(t *testing.T) {
	addr := &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 0,
	}

	tcp, err := net.ListenTCP("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()

	temp, err := ioutil.TempDir("", "tableflip")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(temp)

	socketPath := filepath.Join(temp, "socket")
	unix, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close()

	parent := newFds(l, nil)
	if _, err := parent.ListenWith("1", addr.Network(), addr.String(), func(_, _ string) (net.Listener, error) { return tcp, nil }); err != nil {
		t.Fatal("Can't add listener:", err)
	}
	tcp.Close()

	if _, err := parent.ListenWith("2", "unix", socketPath, func(_, _ string) (net.Listener, error) { return unix.(Listener), nil }); err != nil {
		t.Fatal("Can't add listener:", err)
	}
	unix.Close()

	if _, err := os.Stat(socketPath); err != nil {
		t.Error("Unix.Close() unlinked socketPath:", err)
	}

	child := newFds(l, parent.copy())
	ln, err := child.Listener("1")
	if err != nil {
		t.Fatal("Can't get listener:", err)
	}
	if ln == nil {
		t.Fatal("Missing listener")
	}
	ln.Close()

	child.Remove("2")
	if _, err := os.Stat(socketPath); err == nil {
		t.Error("Remove() did not unlink socketPath")
	}
}

func TestFdsConn(t *testing.T) {
	parent := newFds(l, nil)
	unixConn, err := parent.DialWith("1", "unixgram", "", func(_, _ string) (net.Conn, error) {
		return net.ListenUnixgram("unixgram", &net.UnixAddr{
			Net:  "unixgram",
			Name: "",
		})
	})
	if err != nil {
		t.Fatal("Can't add conn:", err)
	}
	unixConn.Close()
	defer parent.Remove("1")

	child := newFds(l, parent.copy())
	defer child.Remove("1")
	conn, err := child.Conn("1")
	if err != nil {
		t.Fatal("Can't get conn:", err)
	}
	if conn == nil {
		t.Fatal("Missing conn")
	}
	conn.Close()
}

func TestFdsFile(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	parent := newFds(l, nil)
	if _, err := parent.OpenFileWith("test", "test", func(_ string) (*os.File, error) {
		return w, nil
	}); err != nil {
		t.Fatal("Can't add file:", err)
	}
	w.Close()
	defer parent.Remove("test")

	child := newFds(l, parent.copy())
	file, err := child.File("test")
	if err != nil {
		t.Fatal("Can't get file:", err)
	}
	if file == nil {
		t.Fatal("Missing file")
	}
	file.Close()
}

func TestFdsLock(t *testing.T) {
	fds := newFds(l, nil)
	defer fds.closeFds()

	ln, err := fds.ListenWith("1", "tcp", "127.0.0.1:0", net.Listen)
	defer ln.Close()
	if err != nil {
		t.Fatalf("expected no error in unlocked fds: %v", err)
	}

	fds.lockMutations(ErrUpgradeInProgress)
	_, err = fds.ListenWith("1", "tcp", "127.0.0.1:0", net.Listen)
	if err != nil {
		t.Fatalf("expected no error in getting existing listener from locked fds: %v", err)
	}
	if _, err = fds.Listener("1"); err != nil {
		t.Fatalf("expected no error in getting existing listener from locked fds: %v", err)
	}

	_, err = fds.ListenWith("2", "tcp", "127.0.0.1:0", net.Listen)
	if err != ErrUpgradeInProgress {
		t.Fatalf("expected ErrUpgradeInProgress, got %T %q", err, err)
	}
}
