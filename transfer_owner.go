package tableroll

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"sync"
	"syscall"

	"github.com/inconshreveable/log15"
	"github.com/opencontainers/runc/libcontainer/utils"
	"github.com/pkg/errors"
)

const (
	notifyReady = 42
)

type upgradeSession struct {
	closeOnce   sync.Once
	wr          *net.UnixConn
	coordinator *coordinator
	l           log15.Logger
}

func pidIsDead(osi osIface, pid int) bool {
	proc, _ := osi.FindProcess(pid)
	return proc.Signal(syscall.Signal(0)) != nil
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

	// First get the metadata for fds to expect. This also lets us know how many FDs to get
	fds := []*fd{}

	// Note: use length-prefixing and avoid decoding directly from the socket to
	// ensure the reader isn't put into buffered mode, at which point file
	// descriptors can get lost since go's io buffering is obviously not fd
	// aware.
	var metaJSONLength int32
	if err := binary.Read(s.wr, binary.BigEndian, &metaJSONLength); err != nil {
		return nil, orContextErr(errors.Wrap(err, "protocol error: could not read length of json"))
	}
	metaJSON := make([]byte, metaJSONLength)
	if n, err := io.ReadFull(s.wr, metaJSON); err != nil || n != int(metaJSONLength) {
		return nil, orContextErr(errors.Wrapf(err, "unable to read expected meta json length (expected %v, got (%v, %v))", metaJSONLength, n, err))
	}

	if err := json.Unmarshal(metaJSON, &fds); err != nil {
		return nil, orContextErr(errors.Wrap(err, "can't decode names from owner process"))
	}
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

func (s *upgradeSession) sendReady() error {
	defer s.wr.Close()
	if _, err := s.wr.Write([]byte{notifyReady}); err != nil {
		return errors.Wrap(err, "can't notify owner process")
	}
	s.l.Info("notified the owner process we're ready")
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
