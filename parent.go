package shakiin

import (
	"encoding/gob"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"syscall"

	fdsock "github.com/ftrvxmtrx/fd"
	"github.com/pkg/errors"
)

const (
	notifyReady = 42
)

type parent struct {
	wr          *net.UnixConn
	exited      <-chan error
	coordinator *coordinator
}

func pidIsDead(pid int) bool {
	proc, _ := os.FindProcess(pid)
	return proc.Signal(syscall.Signal(0)) != nil
}

func newParent(coordinationDir string) (*coordinator, *parent, map[fileName]*file, error) {
	coord, err := LockCoordinationDir(coordinationDir)
	if err != nil {
		return nil, nil, nil, err
	}

	sock, err := coord.ConnectParent()
	if err == ErrNoParent {
		return coord, nil, make(map[fileName]*file), nil
	}
	if err != nil {
		return coord, nil, nil, err
	}

	// First get the names of files to expect. This also lets us know how many FDs to get
	var names [][]string
	dec := gob.NewDecoder(sock)
	if err := dec.Decode(&names); err != nil {
		return coord, nil, nil, errors.Wrap(err, "can't decode names from parent process")
	}

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
	sockFiles, err := fdsock.Get(sock, len(sockFileNames), sockFileNames)
	if err != nil {
		return coord, nil, nil, err
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

	// now that we have the FDs from the old parent, we just need to tell it when we're ready and then we're done and happy!

	exited := make(chan error, 1)
	go func() {
		// we should not get anything else on the sock other than a close
		n, err := io.Copy(ioutil.Discard, sock)
		if n != 0 {
			exited <- errors.New("unexpected data from parent process")
		} else if err != nil {
			exited <- errors.Wrap(err, "unexpected error while waiting for previous parent to exit")
		}
		close(exited)
	}()

	return coord, &parent{
		wr:          sock,
		exited:      exited,
		coordinator: coord,
	}, files, nil
}

func (ps *parent) sendReady() error {
	defer ps.wr.Close()
	if _, err := ps.wr.Write([]byte{notifyReady}); err != nil {
		return errors.Wrap(err, "can't notify parent process")
	}
	// Now that we're ready and the old process is draining, take over and relinquish the lock.
	if err := ps.coordinator.BecomeParent(); err != nil {
		return err
	}
	if err := ps.coordinator.Unlock(); err != nil {
		return err
	}
	return nil
}
