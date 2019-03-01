package tableroll

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"

	"github.com/inconshreveable/log15"
	"github.com/opencontainers/runc/libcontainer/utils"
	"github.com/pkg/errors"
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
func passFdsToSibling(l log15.Logger, conn *net.UnixConn, passedFiles map[string]*fd) (*sibling, <-chan error) {
	errChan := make(chan error, 1)
	fds := make([]*fd, 0, len(passedFiles))
	for _, fd := range passedFiles {
		fds = append(fds, fd)
	}

	c := &sibling{
		conn:   conn,
		readyC: make(chan struct{}),
		l:      l,
	}
	go func() {
		defer close(errChan)
		err := c.writeFiles(fds)
		if err != nil {
			errChan <- err
		}
	}()
	return c, errChan
}

// writeFiles passes the list of files to the sibling.
func (c *sibling) writeFiles(fds []*fd) error {
	connFile, err := c.conn.File()
	if err != nil {
		return errors.Wrapf(err, "could not convert sibling connection to file")
	}
	validFds := make([]*fd, 0, len(fds))
	rawFds := make([]*os.File, 0, len(fds))
	for i := range fds {
		fd := fds[i]
		if fd.file == nil {
			continue
		}
		rawFds = append(rawFds, fd.file.File)
		validFds = append(validFds, fd)
	}

	c.l.Info("passing along fds to our sibling", "files", fds)
	var jsonBlob bytes.Buffer
	enc := json.NewEncoder(&jsonBlob)
	if err := enc.Encode(validFds); err != nil {
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
	for _, fi := range rawFds {
		if err := utils.SendFd(connFile, fi.Name(), fi.Fd()); err != nil {
			return fmt.Errorf("could not write fds to sibling: %v", err)
		}
	}

	// Finally, read ready byte and the handoff is done!
	var b [1]byte
	if n, err := c.conn.Read(b[:]); n > 0 && b[0] == notifyReady {
		c.l.Debug("our sibling sent us a ready")
		c.readyC <- struct{}{}
	} else {
		c.l.Debug("our sibling failed to send us a ready", "err", err)
		return errors.Wrapf(err, "sibling did not send us a ready byte: read %v bytes, %v", n, b)
	}
	return nil
}
