package shakiin

import (
	"encoding/gob"
	"net"
	"os"

	fdsock "github.com/ftrvxmtrx/fd"
	"github.com/inconshreveable/log15"
)

type sibling struct {
	readyR, namesW *os.File
	readyC         <-chan *os.File
	exitedC        <-chan error
	doneC          <-chan struct{}
	conn           *net.UnixConn
	l              log15.Logger
}

func (c *sibling) String() string {
	return c.conn.RemoteAddr().String()
}

func startSibling(l log15.Logger, conn *net.UnixConn, passedFiles map[fileName]*file) (*sibling, error) {
	fds := make([]*os.File, 0, len(passedFiles))
	fdNames := make([][]string, 0, len(passedFiles))
	for name, file := range passedFiles {
		nameSlice := make([]string, len(name))
		copy(nameSlice, name[:])
		fdNames = append(fdNames, nameSlice)
		fds = append(fds, file.File)
	}

	doneC := make(chan struct{})
	exitedC := make(chan error, 1)
	readyC := make(chan *os.File, 1)

	c := &sibling{
		conn:    conn,
		readyC:  readyC,
		exitedC: exitedC,
		doneC:   doneC,
		l:       l,
	}
	go c.writeFiles(fdNames, fds)
	go c.waitReady(readyC)
	return c, nil
}

func (c *sibling) waitReady(readyC chan<- *os.File) {
	var b [1]byte
	if n, _ := c.readyR.Read(b[:]); n > 0 && b[0] == notifyReady {
		// We know that writeFiles has finished now.
		// TODO: signal the sibling that we're exiting
		readyC <- c.namesW
	} else {
		c.l.Debug("our sibling failed to send us a ready")
	}
	c.readyR.Close()
}

func (c *sibling) writeFiles(names [][]string, fds []*os.File) {
	c.l.Info("passing along fds to our sibling", "numfds", len(fds))
	enc := gob.NewEncoder(c.conn)
	if names == nil {
		// Gob panics on nil
		_ = enc.Encode([][]string{})
		return
	}
	_ = enc.Encode(names)

	err := fdsock.Put(c.conn, fds...)
	if err != nil {
		// TODO:
		panic(err)
	}
}
