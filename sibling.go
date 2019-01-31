package tableroll

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
	readyC chan struct{}
	conn   *net.UnixConn
	l      log15.Logger
}

func (c *sibling) String() string {
	return c.conn.RemoteAddr().String()
}

// passFdsToSibling passes all this processes file descriptors to a sibling
// over the provided unix connection.  It returns an error channel which will,
// at most, have one error written to it.
func passFdsToSibling(l log15.Logger, conn *net.UnixConn, passedFiles map[fileName]*file) (*sibling, <-chan error) {
	errChan := make(chan error, 1)
	fds := make([]*os.File, 0, len(passedFiles))
	fdNames := make([][]string, 0, len(passedFiles))
	for name, file := range passedFiles {
		nameSlice := make([]string, len(name))
		copy(nameSlice, name[:])
		fdNames = append(fdNames, nameSlice)
		fds = append(fds, file.File)
	}

	c := &sibling{
		conn:   conn,
		readyC: make(chan struct{}),
		l:      l,
	}
	go func() {
		defer close(errChan)
		err := c.writeFiles(fdNames, fds)
		if err != nil {
			errChan <- err
		}
	}()
	return c, errChan
}

// writeFiles passes the list of files to the sibling.
func (c *sibling) writeFiles(names [][]string, fds []*os.File) error {
	c.l.Info("passing along fds to our sibling", "files", fds)
	var jsonBlob bytes.Buffer
	enc := json.NewEncoder(&jsonBlob)
	if names == nil {
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
		return fmt.Errorf("could not write json length to sibling: %v", err)
	}
	if _, err := c.conn.Write(jsonBlob.Bytes()); err != nil {
		return fmt.Errorf("could not write json to sibling: %v", err)
	}

	// Write all files it's expecting
	if err := fdsock.Put(c.conn, fds...); err != nil {
		return fmt.Errorf("could not write fds to sibling: %v", err)
	}

	// Finally, read ready byte and the handoff is done!
	var b [1]byte
	if n, _ := c.conn.Read(b[:]); n > 0 && b[0] == notifyReady {
		c.l.Debug("our sibling sent us a ready")
		c.readyC <- struct{}{}
	} else {
		c.l.Debug("our sibling failed to send us a ready")
		return fmt.Errorf("sibling did not send us a ready byte: %v, %v", n, b)
	}
	return nil
}
