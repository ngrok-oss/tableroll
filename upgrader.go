package tableroll

import (
	"net"
	"strings"
	"sync"
	"time"

	"github.com/inconshreveable/log15"
	"github.com/pkg/errors"
)

// DefaultUpgradeTimeout is the duration in which the upgrader expects the
// sibling to send a 'Ready' notification after passing over all its file
// descriptors; If the sibling does not send that it is ready in that duration,
// this Upgrader will close the sibling's connection and wait for additional connections.
const DefaultUpgradeTimeout time.Duration = time.Minute

// Upgrader handles zero downtime upgrades and passing files between processes.
type Upgrader struct {
	upgradeTimeout time.Duration

	parent     *parent
	coord      *coordinator
	readyOnce  sync.Once
	readyC     chan struct{}
	stopOnce   sync.Once
	stopC      chan struct{}
	upgradeSem chan struct{}
	exitC      chan struct{} // only close this if holding upgradeSem

	upgradeSock *net.UnixListener

	l log15.Logger

	Fds *Fds

	// mocks
	os osIface
}

// Option is an option function for Upgrader.
// See Rob Pike's post on the topic for more information on this pattern:
// https://commandcenter.blogspot.com/2014/01/self-referential-functions-and-design.html
type Option func(u *Upgrader)

// WithUpgradeTimeout allows configuring the update timeout. If a time of 0 is
// specified, the default will be used.
func WithUpgradeTimeout(t time.Duration) Option {
	return func(u *Upgrader) {
		u.upgradeTimeout = t
		if u.upgradeTimeout <= 0 {
			u.upgradeTimeout = DefaultUpgradeTimeout
		}
	}
}

// WithLogger configures the logger to use for tableroll operations.
// By default, nothing will be logged.
func WithLogger(l log15.Logger) Option {
	return func(u *Upgrader) {
		u.l = l
	}
}

// New constructs a tableroll upgrader.
// The first argument is a directory. All processes in an upgrade chain must
// use the same coordination directory. The provided directory must exist and
// be writeable by the process using tableroll.
// Canonically, this directory is `/run/${program}/tableroll/`.
// Any number of options to configure tableroll may also be provided.
func New(coordinationDir string, opts ...Option) (*Upgrader, error) {
	return newUpgrader(realOS{}, coordinationDir, opts...)
}

func newUpgrader(os osIface, coordinationDir string, opts ...Option) (*Upgrader, error) {
	upgradeListener, err := listenSock(os, coordinationDir)
	if err != nil {
		return nil, errors.Wrapf(err, "error listening on upgrade socket")
	}

	noopLogger := log15.New()
	noopLogger.SetHandler(log15.DiscardHandler())
	s := &Upgrader{
		upgradeTimeout: DefaultUpgradeTimeout,
		readyC:         make(chan struct{}),
		stopC:          make(chan struct{}),
		upgradeSem:     make(chan struct{}, 1),
		upgradeSock:    upgradeListener,
		exitC:          make(chan struct{}),
		l:              noopLogger,
		os:             os,
	}
	for _, opt := range opts {
		opt(s)
	}

	coord, parent, files, err := newParent(s.l, s.os, coordinationDir)
	if err != nil {
		return nil, err
	}
	s.coord = coord
	s.parent = parent
	s.Fds = newFds(s.l, files)

	go func() {
		for {
			err := s.awaitUpgrade()
			if err != nil {
				if err == errClosed {
					s.l.Info("upgrade socket closed, no longer listening for upgrades")
					return
				}
				s.l.Error("error awaiting upgrade", "err", err)
			}
		}
	}()

	return s, nil
}

func listenSock(osi osIface, coordinationDir string) (*net.UnixListener, error) {
	listenpath := upgradeSockPath(coordinationDir, osi.Getpid())
	return net.ListenUnix("unix", &net.UnixAddr{
		Name: listenpath,
		Net:  "unix",
	})
}

var errClosed = errors.New("connection closed")

func (u *Upgrader) awaitUpgrade() error {
	for {
		conn, err := u.upgradeSock.AcceptUnix()
		if err != nil {
			if strings.Contains(err.Error(), "use of closed network connection") {
				return errClosed
			}
			return errors.Wrap(err, "error accepting upgrade socket request")
		}
		defer conn.Close()

		// We got a request, only handle one request at a time via semaphore..
		// Acquire semaphore, but don't block. This allows informing
		// the user that they are doing too many upgrade requests.
		select {
		default:
			return errors.New("upgrade in progress")
		case u.upgradeSem <- struct{}{}:
		}

		defer func() {
			<-u.upgradeSem
		}()

		// Make sure we're still ok to perform an upgrade.
		select {
		case <-u.exitC:
			return errors.New("already upgraded")
		default:
		}

		select {
		case <-u.readyC:
		default:
			return errors.New("this process cannot service an ugprade request until it is ready; not yet marked ready")
		}

		u.l.Info("passing along the torch")
		// time to pass our FDs along
		nextParent, err := startSibling(u.l, conn, u.Fds.copy())
		if err != nil {
			return errors.Wrap(err, "can't start next parent")
		}

		readyTimeout := time.After(u.upgradeTimeout)
		select {
		case <-u.stopC:
			return errors.New("terminating")
		case <-readyTimeout:
			return errors.Errorf("new parent %s timed out", nextParent)
		case <-nextParent.readyC:
			u.l.Info("next parent is ready, marking ourselves as up for exit")
			close(u.exitC)
			return nil
		}
	}
}

// Ready signals that the current process is ready to accept connections.
// It must be called to finish the upgrade.
//
// All fds which were inherited but not used are closed after the call to Ready.
func (u *Upgrader) Ready() error {
	u.readyOnce.Do(func() {
		u.Fds.closeInherited()
		close(u.readyC)
	})

	if u.parent == nil {
		// we are the parent!
		if err := u.coord.BecomeParent(); err != nil {
			return err
		}
		if err := u.coord.Unlock(); err != nil {
			return err
		}
		return nil
	}
	return u.parent.sendReady()
}

// Exit returns a channel which is closed when the process should
// exit.
func (u *Upgrader) Exit() <-chan struct{} {
	return u.exitC
}

// Stop prevents any more upgrades from happening, and closes
// the exit channel.
func (u *Upgrader) Stop() {
	u.stopOnce.Do(func() {
		// Interrupt any running Upgrade(), and
		// prevent new upgrade from happening.
		close(u.stopC)
		u.upgradeSem <- struct{}{}
		u.upgradeSock.Close()
		select {
		case <-u.exitC:
		default:
			close(u.exitC)
		}
		<-u.upgradeSem

		u.l.Info("closing file descriptors")
		u.Fds.closeUsed()
	})
}
