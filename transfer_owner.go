package tableroll

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"syscall"

	fdsock "github.com/ftrvxmtrx/fd"
	"github.com/inconshreveable/log15"
	"github.com/pkg/errors"
)

const (
	notifyReady = 42
)

type upgradeSession struct {
	wr          *net.UnixConn
	coordinator *coordinator
	l           log15.Logger
}

func pidIsDead(osi osIface, pid int) bool {
	proc, _ := osi.FindProcess(pid)
	return proc.Signal(syscall.Signal(0)) != nil
}

func connectToCurrentOwner(ctx context.Context, l log15.Logger, osi osIface, coordinationDir string) (*upgradeSession, error) {
	coord, err := lockCoordinationDir(ctx, osi, l, coordinationDir)
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
func (s *upgradeSession) getFiles(ctx context.Context) (map[fileName]*file, error) {
	s.l.Info("getting fds")
	if !s.hasOwner() {
		s.l.Info("no connection present, no files from owner")
		return nil, nil
	}

	functionEnd := make(chan struct{})
	go func() {
		select {
		case <-functionEnd:
		case <-ctx.Done():
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

	// First get the names of files to expect. This also lets us know how many FDs to get
	var names [][]string

	// Note: use length-prefixing and avoid decoding directly from the socket to
	// ensure the reader isn't put into buffered mode, at which point file
	// descriptors can get lost since go's io buffering is obviously not fd
	// aware.
	var jsonNameLength int32
	if err := binary.Read(s.wr, binary.BigEndian, &jsonNameLength); err != nil {
		return nil, orContextErr(errors.Wrap(err, "protocol error: could not read length of json"))
	}
	nameJSON := make([]byte, jsonNameLength)
	if n, err := io.ReadFull(s.wr, nameJSON); err != nil || n != int(jsonNameLength) {
		return nil, orContextErr(errors.Wrapf(err, "unable to read expected name json length (expected %v, got (%v, %v))", jsonNameLength, n, err))
	}

	if err := json.Unmarshal(nameJSON, &names); err != nil {
		return nil, orContextErr(errors.Wrap(err, "can't decode names from owner process"))
	}
	s.l.Debug("expecting files", "names", names)

	// Now grab all the FDs from the owner from the socket
	files := make(map[fileName]*file, len(names))
	sockFileNames := make([]string, 0, len(files))
	for _, parts := range names {
		// parts[2] is the 'addr', which is the best we've got for a filename.
		// TODO(euank): should we just use 'key.String()' like is used in newFile?
		// I want to check this by seeing what the 'filename' is on each end and if
		// it changes from the owner ith how I have this.
		sockFileNames = append(sockFileNames, parts[2])
	}
	sockFiles, err := fdsock.Get(s.wr, len(sockFileNames), sockFileNames)
	if err != nil {
		return nil, orContextErr(err)
	}
	if len(sockFiles) != len(names) {
		panic(errors.Errorf("got %v sockfiles, but expected %v: %+v; %+v", len(sockFiles), len(names), sockFiles, names))
	}
	for i, parts := range names {
		var key fileName
		copy(key[:], parts)

		files[key] = &file{
			os.NewFile(sockFiles[i].Fd(), key.String()),
			sockFiles[i].Fd(),
		}
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
	// Now that we're ready and the old process is draining, take over and relinquish the lock.
	if err := s.coordinator.BecomeOwner(); err != nil {
		return err
	}
	if err := s.coordinator.Unlock(); err != nil {
		return err
	}
	s.l.Info("unlocked coordinator directory")
	return nil
}

func (s *upgradeSession) BecomeOwner() error {
	return s.coordinator.BecomeOwner()
}

func (s *upgradeSession) Close() error {
	if s.wr != nil {
		if err := s.wr.Close(); err != nil {
			s.l.Warn("unable to close unix socket to owner", "err", err)
		}
	}
	return s.coordinator.Unlock()
}
