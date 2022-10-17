package tableroll

import (
	"fmt"
	"net"
	"time"

	"github.com/inconshreveable/log15"
	"github.com/ngrok-oss/tableroll/v2/internal/proto"
	"github.com/pkg/errors"
)

type sibling struct {
	readyC chan struct{}
	conn   *net.UnixConn
	l      log15.Logger
}

func newSibling(l log15.Logger, conn *net.UnixConn) *sibling {
	return &sibling{
		conn: conn,
		l:    l,
	}
}

func (s *sibling) String() string {
	return s.conn.RemoteAddr().String()
}

// passFdsToSibling passes all this processes file descriptors to a sibling
// over the provided unix connection.  It returns an error if it was unable to
// pass all file descriptors along, or if the receiver did not signal that they were received.
// This method also waits for the new process to signal that it intends to take
// over ownership of those file descriptors.
func (s *sibling) giveFDs(readyTimeoutC <-chan time.Time, passedFiles map[string]*fd) error {
	fds := make([]*fd, 0, len(passedFiles))
	for _, fd := range passedFiles {
		fds = append(fds, fd)
	}

	connFile, err := s.conn.File()
	if err != nil {
		return errors.Wrapf(err, "could not convert sibling connection to file")
	}
	defer connFile.Close()

	functionEnd := make(chan struct{})
	defer close(functionEnd)
	go func() {
		select {
		case <-functionEnd:
		case <-readyTimeoutC:
			select {
			case <-functionEnd:
			default:
				s.l.Info("timed out, closing file and connection")
				// fail reads/writes on timeout
				s.conn.Close()
				connFile.Close()
			}
		}
	}()

	validFds := make([]*fd, 0, len(fds))
	for i := range fds {
		fd := fds[i]
		if fd.file == nil {
			continue
		}
		validFds = append(validFds, fd)
	}

	s.l.Info("passing along fds to our sibling", "files", fds)
	if err := proto.WriteVersionedJSONBlob(s.conn, validFds, proto.Version); err != nil {
		return fmt.Errorf("error writing json to sibling: %v", err)
	}

	// Write all files it's expecting
	for _, fi := range validFds {
		if err := sendFile(connFile, fi.file); err != nil {
			return fmt.Errorf("could not write fds to sibling: %v", err)
		}
	}

	return s.awaitReady()
}

func (s *sibling) awaitReady() error {
	// Finally, read ready byte and the handoff is done!
	var b [1]byte
	n, err := s.conn.Read(b[:])
	switch {
	case n > 0 && b[0] == proto.V0NotifyReady:
		s.l.Debug("our sibling sent us a v0 ready")
		return nil
	case n > 0 && b[0] == proto.V1StartReadyHandshake:
		return s.readyHandshake()
	default:
		s.l.Debug("our sibling failed to send us a ready", "err", err)
		return errors.Wrapf(err, "sibling did not send us a ready byte: read %v bytes, %v", n, b)
	}
}

func (s *sibling) readyHandshake() error {
	var vInfo proto.VersionInformation
	err := proto.ReadJSONBlob(s.conn, &vInfo)
	if err != nil {
		return err
	}
	// We told our sibling our version via encoding it in the versioned json blob
	// of files, so it should speak a version we know. If it doesn't, that mean's
	// it's a misbehaving client.
	if vInfo.Version != proto.Version {
		return fmt.Errorf("unable to transfer ownership: unexpected protocol version: %v", vInfo.Version)
	}
	// Send back that we're stepping down, return nil which causes us to step down.
	err = proto.WriteJSONBlob(s.conn, proto.Message{
		Msg: proto.V1MessageSteppingDown,
	})
	if err != nil {
		// We can't be totally sure in this case if the new owner received our message or not.
		// Assume that they did and we should step down, so just log an error and
		// still return nil to 'happily' step down.
		// Zero owners is better than two owners.
		s.l.Error("error sending stepping down message", "err", err)
	}
	return nil
}
