package tableroll

import (
	"fmt"
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

	dir       string
	session   *upgradeSession
	readyOnce sync.Once
	stopOnce  sync.Once

	stateLock sync.Mutex
	state     upgraderState

	// upgradeCompleteC is closed when this upgrader has serviced an upgrade and
	// is no longer the owner of its Fds.
	// This also occurs when `Stop` is called.
	upgradeCompleteC chan struct{}

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
	u := &Upgrader{
		upgradeTimeout:   DefaultUpgradeTimeout,
		state:            upgraderStateCheckingOwner,
		upgradeSock:      upgradeListener,
		upgradeCompleteC: make(chan struct{}),
		l:                noopLogger,
		os:               os,
		dir:              coordinationDir,
	}
	for _, opt := range opts {
		opt(u)
	}

	go u.serveUpgrades()

	_, err = u.becomeOwner()

	return u, err
}

// BecomeOwner upgrades the calling process to the 'owner' of all file descriptors.
// It returns 'true' if it coordinated taking ownership from a previous,
// existing owner process.
// It returns 'false' if it has taken ownership by identifying that no other
// owner existed.
func (u *Upgrader) becomeOwner() (bool, error) {
	sess, err := connectToCurrentOwner(u.l, u.os, u.dir)
	if err != nil {
		return false, err
	}
	u.session = sess
	files, err := sess.getFiles()
	if err != nil {
		sess.Close()
		return false, err
	}
	u.Fds = newFds(u.l, files)
	return sess.hasOwner(), nil
}

func listenSock(osi osIface, coordinationDir string) (*net.UnixListener, error) {
	listenpath := upgradeSockPath(coordinationDir, osi.Getpid())
	return net.ListenUnix("unix", &net.UnixAddr{
		Name: listenpath,
		Net:  "unix",
	})
}

var errClosed = errors.New("connection closed")

func (u *Upgrader) serveUpgrades() {
	for {
		conn, err := u.upgradeSock.AcceptUnix()
		if err != nil {
			if strings.Contains(err.Error(), "use of closed network connection") {
				u.l.Info("upgrade socket closed, no longer listening for upgrades")
				return
			}
			u.l.Error("error awaiting upgrade", "err", err)
			continue
		}
		go u.handleUpgradeRequest(conn)
	}
}

func (u *Upgrader) transitionTo(state upgraderState) error {
	u.stateLock.Lock()
	defer u.stateLock.Unlock()
	return u.state.transitionTo(state)
}

func (u *Upgrader) mustTransitionTo(state upgraderState) {
	u.stateLock.Lock()
	defer u.stateLock.Unlock()
	if err := u.state.transitionTo(state); err != nil {
		panic(fmt.Sprintf("BUG: error transitioning to %q: %v", state, err))
	}
}

func (u *Upgrader) handleUpgradeRequest(conn *net.UnixConn) {
	defer conn.Close()

	if err := u.transitionTo(upgraderStateTransferringOwnership); err != nil {
		u.l.Info("cannot handle upgrade request", "reason", err)
		return
	}

	u.l.Info("handling an upgrade request from peer")
	// time to pass our FDs along
	nextOwner, errC := passFdsToSibling(u.l, conn, u.Fds.copy())

	readyTimeout := time.NewTimer(u.upgradeTimeout)
	defer readyTimeout.Stop()
	select {
	case err := <-errC:
		u.l.Error("failed to pass file descriptors to next owner", "reason", "error", "err", err)
		// remain owner
		if err := u.transitionTo(upgraderStateOwner); err != nil {
			// could happen if 'Stop' was called after 'handleUpgradeRequest'
			// started, and then the request failed.
			// This leaves us in the state of being the sole owner of Fds, but not
			// being able to pass on ownership because that's what 'Stop' indicates
			// is desired.
			// At this point, we can't really do anything but complain.
			u.l.Error("unable to remain owner after upgrade failure", "err", err)
		}
	case <-readyTimeout.C:
		u.l.Error("failed to pass file descriptors to next owner", "reason", "timeout")
		if err := u.transitionTo(upgraderStateOwner); err != nil {
			u.l.Error("unable to remain owner after upgrade timeout", "err", err)
		}
	case <-nextOwner.readyC:
		u.l.Info("next owner is ready, marking ourselves as up for exit")
		// ignore error, if we were 'Stopped' we can't transition, but we also
		// don't care.
		_ = u.transitionTo(upgraderStateDraining)
		close(u.upgradeCompleteC)
	}
}

// Ready signals that the current process is ready to accept connections.
// It must be called to finish the upgrade.
//
// All fds which were inherited but not used are closed after the call to Ready.
func (u *Upgrader) Ready() error {
	u.stateLock.Lock()
	defer u.stateLock.Unlock()

	u.readyOnce.Do(func() {
		u.Fds.closeInherited()
	})

	if !u.session.hasOwner() {
		// If we can't find a owner to request listeners from, then just assume we
		// are the owner.
		defer func() {
			// unlock the coordination dir even if we fail to become the owner, this
			// gives another process a chance at it even if our caller for some
			// reason decides to not panic/exit
			if err := u.session.Close(); err != nil {
				u.l.Error("error closing upgrade session", "err", err)
			}
		}()
		err := u.session.BecomeOwner()
		if err != nil {
			return err
		}
		return u.state.transitionTo(upgraderStateOwner)
	}
	if err := u.session.sendReady(); err != nil {
		return err
	}
	if err := u.state.transitionTo(upgraderStateOwner); err != nil {
		return err
	}
	return nil
}

// UpgradeComplete returns a channel which is closed when the managed file
// descriptors have been passed to the next process, and the next process has
// indicated it is ready.
func (u *Upgrader) UpgradeComplete() <-chan struct{} {
	return u.upgradeCompleteC
}

// Stop prevents any more upgrades from happening, and closes
// the upgrade complete channel.
// It also closes any file descriptors in Fds which were inherited but are
// unused.
func (u *Upgrader) Stop() {
	u.mustTransitionTo(upgraderStateStopped)
	u.stopOnce.Do(func() {
		// Interrupt any running Upgrade(), and
		// prevent new upgrade from happening.
		u.upgradeSock.Close()
		select {
		case <-u.upgradeCompleteC:
		default:
			close(u.upgradeCompleteC)
		}

		u.l.Info("closing file descriptors")
		u.Fds.closeUsed()
	})
}
