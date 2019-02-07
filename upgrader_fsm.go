package tableroll

import "fmt"

// upgraderState represents a small finite state machine. It has the following transitions:
// ∅                     → CheckingOwner
// CheckingOwnership     → AwaitingOwnership
// CheckingOwnership     → Owner
// AwaitingOwnership     → Owner
// Owner                 → TransferringOwnership
// TransferringOwnership → Owner
// TransferringOwnership → Draining
//
// The meaning of each state is described above the state's definition below.
type upgraderState string

const (
	// CheckingOwnership is the initial state. It indicates this upgrader is
	// trying to connect to the current owner to determine if they exist.
	upgraderStateCheckingOwner upgraderState = "checking-owner"
	// Owner is the state of an upgrader that has successfully either upgraded or
	// determined that it is the sole process and should thus take ownership.
	upgraderStateOwner = "owner"
	// TransferringOwnership is the state of an upgrader that has received a
	// request from a new process to pass over its FDs, but either has not passed
	// them all over, or has not yet received a ready.
	upgraderStateTransferringOwnership = "transferring-ownership"
	// Draining is the state a process is in after a new owner has taken over.
	upgraderStateDraining = "draining"
	// Stopped is the state a process is in after it has completed draining or
	// has been marked to stop.
	upgraderStateStopped = "stopped"
)

var validTransitions = map[upgraderState][]upgraderState{
	upgraderStateCheckingOwner: []upgraderState{
		upgraderStateOwner,
		upgraderStateStopped,
	},
	upgraderStateOwner: []upgraderState{
		upgraderStateTransferringOwnership,
		upgraderStateStopped,
	},
	upgraderStateTransferringOwnership: []upgraderState{
		upgraderStateOwner,
		upgraderStateDraining,
		upgraderStateStopped,
	},
	upgraderStateDraining: []upgraderState{
		upgraderStateDraining,
		upgraderStateStopped,
	},
	upgraderStateStopped: []upgraderState{
		upgraderStateStopped,
	},
}

func (u *upgraderState) canTransitionTo(state upgraderState) error {
	validTargets := validTransitions[*u]

	for _, target := range validTargets {
		if target == state {
			return nil
		}
	}
	return fmt.Errorf("unable to transition from %s to %s", *u, state)
}

func (u *upgraderState) transitionTo(state upgraderState) error {
	if err := u.canTransitionTo(state); err != nil {
		return err
	}
	*u = state
	return nil
}
