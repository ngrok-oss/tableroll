package tableroll

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
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

func connectToParent(l log15.Logger, osi osIface, coordinationDir string) (*upgradeSession, error) {
	coord, err := lockCoordinationDir(osi, l, coordinationDir)
	if err != nil {
		return nil, err
	}

	sess := &upgradeSession{
		coordinator: coord,
		l:           l,
	}

	// sock is used for all messages between two siblings
	sock, err := coord.ConnectParent()
	if err == errNoParent {
		return sess, nil
	}
	if err != nil {
		sess.Close()
		return nil, err
	}
	sess.wr = sock
	return sess, nil
}

func (s *upgradeSession) hasParent() bool {
	return s.wr != nil
}

func (s *upgradeSession) getFiles() (map[fileName]*file, error) {
	s.l.Info("getting fds")
	if !s.hasParent() {
		s.l.Info("no parent connection present, no files from parent")
		return nil, nil
	}

	// First get the names of files to expect. This also lets us know how many FDs to get
	var names [][]string

	// Note: use length-prefixing and avoid decoding directly from the socket to
	// ensure the reader isn't put into buffered mode, at which point file
	// descriptors can get lost since go's io buffering is obviously not fd
	// aware.
	var jsonNameLength int32
	if err := binary.Read(s.wr, binary.BigEndian, &jsonNameLength); err != nil {
		return nil, fmt.Errorf("protocol error: could not read length of json: %v", err)
	}
	nameJSON := make([]byte, jsonNameLength)
	if n, err := io.ReadFull(s.wr, nameJSON); err != nil || n != int(jsonNameLength) {
		return nil, fmt.Errorf("unable to read expected name json length (expected %v, got (%v, %v))", jsonNameLength, n, err)
	}

	if err := json.Unmarshal(nameJSON, &names); err != nil {
		return nil, errors.Wrap(err, "can't decode names from parent process")
	}
	s.l.Debug("expecting files", "names", names)

	// Now grab all the FDs from the parent from the socket
	files := make(map[fileName]*file, len(names))
	sockFileNames := make([]string, 0, len(files))
	for _, parts := range names {
		// parts[2] is the 'addr', which is the best we've got for a filename.
		// TODO(euank): should we just use 'key.String()' like is used in newFile?
		// I want to check this by seeing what the 'filename' is on each end and if
		// it changes from the parent process to the next parent with how I have this.
		sockFileNames = append(sockFileNames, parts[2])
	}
	sockFiles, err := fdsock.Get(s.wr, len(sockFileNames), sockFileNames)
	if err != nil {
		return nil, err
	}
	if len(sockFiles) != len(names) {
		panic(fmt.Errorf("got %v sockfiles, but expected %v: %+v; %+v", len(sockFiles), len(names), sockFiles, names))
	}
	for i, parts := range names {
		var key fileName
		copy(key[:], parts)

		files[key] = &file{
			os.NewFile(sockFiles[i].Fd(), key.String()),
			sockFiles[i].Fd(),
		}
	}
	s.l.Info("got fds from old parent", "files", files)
	return files, nil
}

func (s *upgradeSession) sendReady() error {
	defer s.wr.Close()
	if _, err := s.wr.Write([]byte{notifyReady}); err != nil {
		return errors.Wrap(err, "can't notify parent process")
	}
	s.l.Info("notified the parent process we're ready")
	// Now that we're ready and the old process is draining, take over and relinquish the lock.
	if err := s.coordinator.BecomeParent(); err != nil {
		return err
	}
	if err := s.coordinator.Unlock(); err != nil {
		return err
	}
	s.l.Info("unlocked coordinator directory")
	return nil
}

func (s *upgradeSession) BecomeParent() error {
	return s.coordinator.BecomeParent()
}

func (s *upgradeSession) Close() error {
	if s.wr != nil {
		if err := s.wr.Close(); err != nil {
			s.l.Warn("unable to close unix socket to parent", "err", err)
		}
	}
	return s.coordinator.Unlock()
}
