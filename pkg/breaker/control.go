package breaker

import (
	"errors"
	"fmt"
	"time"
)

// Command is a single remote control request issued to the breaker via
// select-before-operate.
type Command struct {
	// Target is the desired end position: PositionOpen or PositionClosed.
	Target Position
	// Source identifies the operator or system issuing the command. It is
	// informational only — this package performs no authentication or
	// authorization.
	Source    string
	Timestamp time.Time
}

// SelectionID identifies an in-progress select-before-operate sequence
// returned by BreakerController.Select. It is opaque and comparable, and
// has a bounded lifetime: a selection not followed by Operate or Cancel
// before its timeout expires automatically and is freed.
type SelectionID string

// ErrInvalidCommand is returned when a Command is malformed for the
// operation being performed — e.g. Select with a Target that is neither
// PositionOpen nor PositionClosed, or an Operate whose Target does not
// match the Command that was selected.
var ErrInvalidCommand = errors.New("breaker: invalid command")

// SelectionExpiredError reports that a SelectionID was valid but has since
// expired (its selection timeout elapsed before Operate or Cancel). Use
// [errors.AsType] to distinguish it from an unrelated error:
//
//	if expired, ok := errors.AsType[*breaker.SelectionExpiredError](err); ok {
//		// re-select and retry
//	}
type SelectionExpiredError struct {
	// ID is the selection that expired.
	ID SelectionID
}

// Error implements the error interface.
func (e *SelectionExpiredError) Error() string {
	return fmt.Sprintf("breaker: selection %s expired", e.ID)
}

// UnknownSelectionError reports that a SelectionID was never issued (or has
// aged out of the expired-selection tombstone kept for
// SelectionExpiredError reporting). Use [errors.AsType] to distinguish it
// from an unrelated error.
type UnknownSelectionError struct {
	// ID is the selection that was never issued (or has aged out of the tombstone).
	ID SelectionID
}

// Error implements the error interface.
func (e *UnknownSelectionError) Error() string {
	return fmt.Sprintf("breaker: unknown selection %s", e.ID)
}

// InterlockError reports that a control operation was refused because of an
// interlock — e.g. a selection is already in progress, the breaker
// mechanism is already in motion, or an automatic reclose sequence is
// active. Use [errors.AsType] to distinguish it from an unrelated error:
//
//	if lock, ok := errors.AsType[*breaker.InterlockError](err); ok {
//		log.Printf("rejected: %s", lock.Reason)
//	}
type InterlockError struct {
	// Reason describes which interlock rejected the operation.
	Reason string
}

// Error implements the error interface.
func (e *InterlockError) Error() string {
	return fmt.Sprintf("breaker: interlocked: %s", e.Reason)
}
