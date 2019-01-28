package shakiin

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"

	fdsock "github.com/ftrvxmtrx/fd"
	"github.com/inconshreveable/log15"
)

type sibling struct {
	readyR, namesW *os.File
	readyC         chan *os.File
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
	return c, nil
}

func (c *sibling) waitReady(readyC chan<- *os.File) {
	var b [1]byte
	if n, _ := c.conn.Read(b[:]); n > 0 && b[0] == notifyReady {
		c.l.Debug("our sibling sent us a ready")
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
	var jsonBlob bytes.Buffer
	enc := json.NewEncoder(&jsonBlob)
	if names == nil {
		// Gob panics on nil
		names = [][]string{}
	}
	if err := enc.Encode(names); err != nil {
		panic(err)
	}

	var jsonBlobLenBuf bytes.Buffer
	if err := binary.Write(&jsonBlobLenBuf, binary.BigEndian, int32(jsonBlob.Len())); err != nil {
		panic(fmt.Errorf("could not binary encode an int32: %v", err))
	}
	if jsonBlobLenBuf.Len() != 4 {
		panic(fmt.Errorf("int32 should be 4 bytes, not: %+v", jsonBlobLenBuf))
	}

	// Length-prefixed json blob
	if _, err := c.conn.Write(jsonBlobLenBuf.Bytes()); err != nil {
		panic("TODO(euank): real error handling: " + err.Error())
	}
	if _, err := c.conn.Write(jsonBlob.Bytes()); err != nil {
		panic("TODO(euank): real error handling 2: " + err.Error())
	}

	// Write all files it's expecting
	if err := fdsock.Put(c.conn, fds...); err != nil {
		// TODO: this should *not* be a panic for sure
		panic(err)
	}

	// Finally, read ready byte and the handoff is done!
	var b [1]byte
	if n, _ := c.conn.Read(b[:]); n > 0 && b[0] == notifyReady {
		c.l.Debug("our sibling sent us a ready")
		// We know that writeFiles has finished now.
		// TODO: signal the sibling that we're exiting
		c.readyC <- c.namesW
	} else {
		c.l.Debug("our sibling failed to send us a ready")
	}
	c.readyR.Close()
}
