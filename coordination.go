package tableroll

import (
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/inconshreveable/log15"
	"github.com/pkg/errors"
	"github.com/rkt/rkt/pkg/lock"
	"k8s.io/utils/clock"
)

// errNoOwner indicates that either no process currently is marked as
// controlling the upgradeable file descriptors (e.g. initial startup case), or
// a process is supposed to own them but is dead (e.g. it crashed).
var errNoOwner = errors.New("no owner process exists")

// coordination is used to coordinate between N processes, one of which is the
// current owner.
// It must provide means of getting the owner, updating the owner, and.
// ensuring it has unique ownership of that information for the duration
// between a read and update.
// It is implemented in this case with unix locks on a file.
type coordinator struct {
	lock *lock.FileLock
	dir  string
	id   string
	l    log15.Logger

	// mocks
	clock clock.Clock
}

func newCoordinator(clock clock.Clock, l log15.Logger, dir string, id string) *coordinator {
	l = l.New("dir", dir)
	coord := &coordinator{dir: dir, l: l, clock: clock, id: id}
	return coord
}

func (c *coordinator) Listen(ctx context.Context) (*net.UnixListener, error) {
	listenpath := upgradeSockPath(c.dir, c.id)
	l, err := (&net.ListenConfig{}).Listen(ctx, "unix", listenpath)
	if err != nil {
		return nil, err
	}
	return l.(*net.UnixListener), nil
}

func touchFile(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0755)
	f.Close()
	return err
}

// Lock takes an exclusive lock on the given coordination directory.  If the
// directory is already locked, the function will block until the lock can be
// acquired, or until the passed context is cancelled.
func (c *coordinator) Lock(ctx context.Context) error {
	idPath := c.idFile()
	if err := touchFile(idPath); err != nil {
		return err
	}
	c.l.Info("taking lock on coordination dir")
	flock, err := lock.NewLock(idPath, lock.RegFile)
	if err != nil {
		return err
	}
	for ctx.Err() == nil {
		err := flock.TryExclusiveLock()
		if err == nil {
			// lock get
			break
		}
		if err != lock.ErrLocked {
			return errors.Wrap(err, "error trying to lock coordination directory")
		}
		// lock busy, wait and try again
		c.clock.Sleep(100 * time.Millisecond)
	}
	c.l.Info("took lock on coordination dir")
	c.lock = flock
	return ctx.Err()
}

func (c *coordinator) idFile() string {
	// named 'pid' for historical reasons, originally the opaque id was always a pid
	return filepath.Join(c.dir, "pid")
}

// BecomeOwner marks this coordinator as the owner of the coordination directory.
// It should only be called while the lock is held.
func (c *coordinator) BecomeOwner() error {
	c.l.Info("writing id to become owner", "id", c.id)
	return ioutil.WriteFile(c.idFile(), []byte(c.id), 0755)
}

// Unlock unlocks the coordination id file
func (c *coordinator) Unlock() error {
	if c.lock == nil {
		c.l.Info("not unlocking coordination dir; not locked")
		return nil
	}
	c.l.Info("unlocking coordination dir")
	return c.lock.Unlock()
}

// GetOwnerID returns the current 'owner' for this coordination directory.
// It will return 'errNoOwner' if there isn't currently an owner.
func (c *coordinator) GetOwnerID() (string, error) {
	c.l.Info("discovering current owner")
	data, err := ioutil.ReadFile(c.idFile())
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		// empty file, that means no owner
		return "", errNoOwner
	}
	c.l.Info("found owner", "owner", string(data))
	return string(data), nil
}

func (c *coordinator) ConnectOwner(ctx context.Context) (*net.UnixConn, error) {
	oid, err := c.GetOwnerID()
	if err != nil {
		return nil, err
	}
	c.l.Info("connecting to owner", "owner", oid)

	sockPath := upgradeSockPath(c.dir, oid)
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
	if err != nil {
		if isContextDialErr(err) {
			return nil, err
		}
		c.l.Warn("found an owner ID, but it wasn't listening; possibly a stale process that crashed?", "oid", oid, "dialErr", err)
		return nil, errNoOwner
	}

	return conn.(*net.UnixConn), nil
}

func isContextDialErr(err error) bool {
	if opErr, ok := err.(*net.OpError); ok {
		err = opErr.Err
	}
	return err == context.Canceled || err == context.DeadlineExceeded
}

func upgradeSockPath(coordinationDir string, oid string) string {
	return filepath.Join(coordinationDir, fmt.Sprintf("%s.sock", oid))
}
