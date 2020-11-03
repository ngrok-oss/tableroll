package tableroll

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/inconshreveable/log15"
	"github.com/ngrok/tableroll/internal/proto"
	"github.com/opencontainers/runc/libcontainer/utils"
	"github.com/pkg/errors"
)

type upgradeSession struct {
	closeOnce    sync.Once
	wr           *net.UnixConn
	coordinator  *coordinator
	ownerVersion uint32
	l            log15.Logger
}

func connectToCurrentOwner(ctx context.Context, l log15.Logger, coord *coordinator) (*upgradeSession, error) {
	err := coord.Lock(ctx)
	if err != nil {
		return nil, err
	}

	sess := &upgradeSession{
		coordinator: coord,
		l:           l,
	}

	// sock is used for all messages between two siblings
	sock, err := coord.ConnectOwner(ctx)
	if err == errNoOwner {
		return sess, nil
	}
	if err != nil {
		sess.Close()
		return nil, err
	}
	sess.wr = sock
	return sess, nil
}

func (s *upgradeSession) hasOwner() bool {
	return s.wr != nil
}

// getFiles retrieves all files over the opened upgrade session. In the case of
// a context error, the upgrade session will be closed and a context error will
// be returned as a wrapped error. The context error may be retreived with
// errors.Cause in that case.
func (s *upgradeSession) getFiles(ctx context.Context) (map[string]*fd, error) {
	s.l.Info("getting fds")
	if !s.hasOwner() {
		s.l.Info("no connection present, no files from owner")
		return nil, nil
	}

	sockFile, err := s.wr.File()
	if err != nil {
		return nil, errors.Wrapf(err, "could not convert sibling connection to file")
	}
	defer sockFile.Close()

	functionEnd := make(chan struct{})
	go func() {
		select {
		case <-functionEnd:
		case <-ctx.Done():
			// double check the function hasn't already returned, if it has then the
			// session's out of our hands already.
			select {
			case <-functionEnd:
				return
			default:
			}
			// if there was a context error, close the socket to cause any pending reads/writes to fail
			s.Close()
		}
	}()
	defer close(functionEnd)
	// orContextErr returns a context error instead of the passed error if there is one.
	// This is done under the assumption that the 'err' passed in was caused by
	// the context cancel/timeout/whatever, and the context error is therefore
	// both more useful for a programmer to check and a more meaningful message.
	orContextErr := func(err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return errors.Wrap(ctxErr, err.Error())
		}
		return err
	}

	fds := []*fd{}
	version, err := proto.ReadVersionedJSONBlob(s.wr, &fds)
	if err != nil {
		return nil, orContextErr(errors.Wrap(err, "can't read fd metadata from owner process"))
	}
	s.ownerVersion = version

	s.l.Debug("expecting files", "fds", fds)
	// Now grab all the FDs from the owner from the socket
	files := make(map[string]*fd, len(fds))
	sockFileNames := make([]string, 0, len(fds))
	for i := range fds {
		fd := fds[i]
		// parts[2] is the 'addr', which is the best we've got for a filename.
		// TODO(euank): should we just use 'key.String()' like is used in newFile?
		// I want to check this by seeing what the 'filename' is on each end and if
		// it changes from the owner ith how I have this.
		sockFileNames = append(sockFileNames, fd.String())
	}
	sockFiles := make([]*os.File, 0, len(sockFileNames))
	for i := 0; i < len(sockFileNames); i++ {
		file, err := utils.RecvFd(sockFile)
		if err != nil {
			s.l.Error("error receiving a file descriptor", "err", err)
			return nil, orContextErr(errors.Wrap(err, "error getting file descriptors"))
		}
		sockFiles = append(sockFiles, file)
	}
	if len(sockFiles) != len(fds) {
		panic(errors.Errorf("got %v sockfiles, but expected %v: %+v; %+v", len(sockFiles), len(fds), sockFiles, fds))
	}
	for i := range fds {
		fd := fds[i]
		fd.associateFile(fd.String(), sockFiles[i])

		files[fd.ID] = fd
	}
	s.l.Info("got fds from old owner", "files", files)
	return files, nil
}

func (s *upgradeSession) readyHandshake() error {
	defer s.wr.Close()
	if s.ownerVersion == 0 {
		s.l.Info("performing v0 ready handshake")
		if _, err := s.wr.Write([]byte{proto.V0NotifyReady}); err != nil {
			return errors.Wrap(err, "can't notify owner process")
		}
		s.l.Info("notified the owner process we're ready")
		return nil
	}
	s.l.Info("performing v1 ready handshake")
	// The owner we're taking over from indicated it speaks v1+ of the file
	// descriptor handoff protocol.
	// That means we can do a proper handshake rather than writing and forgetting.
	// Due to the fact that the unix socket we're using is a stream socket, not a
	// datagram socket, the fact that we write a ready value doesn't actually
	// mean the owner has read it yet.
	// We need to wait for an ack so we know our owner read it before we consider
	// ourselves the new owner.
	// First write a v1 start ready handshake byte. This is because the owner
	// told us it can speak v1+, but we haven't indicated our verison yet, so it
	// has to read a byte at the beginning just in case we're v0.
	// Write a byte that indicates to it we're v1+, and then write proper version
	// information.
	if _, err := s.wr.Write([]byte{proto.V1StartReadyHandshake}); err != nil {
		return errors.Wrap(err, "can't notify owner process")
	}
	// now write our explicit version information so it knows to perform a v1
	// handshake
	if err := proto.WriteJSONBlob(s.wr, proto.VersionInformation{
		Version: proto.Version,
	}); err != nil {
		return err
	}
	// Now they know we're v1, they'll ack that we wrote the version with a
	// 'SteppingDown' response
	var obj proto.Message
	err := proto.ReadJSONBlob(s.wr, &obj)
	if err != nil {
		return err
	}
	if obj.Msg != proto.V1MessageSteppingDown {
		return fmt.Errorf("expected stepping down message, got %v", obj.Msg)
	}
	// at this point they acked us, we can become the owner safely.
	return nil
}

func (s *upgradeSession) BecomeOwner() error {
	return s.coordinator.BecomeOwner()
}

func (s *upgradeSession) Close() error {
	var err error
	s.closeOnce.Do(func() {
		if s.wr != nil {
			s.wr.Close()
		}
		err = s.coordinator.Unlock()
	})
	return err
}
