package jobs

import "errors"

// permanentPrefix marks dead-lettered jobs in jobs.last_error so operators can
// tell a permanent failure from an exhausted retry budget.
const permanentPrefix = "permanent failure (not retried): "

// PermanentError marks a handler failure that no retry can fix, such as a
// payload that does not decode. The worker dead-letters the job immediately
// instead of spending its remaining attempts.
type PermanentError struct {
	Err error
}

// Permanent wraps err so the worker dead-letters the job on this attempt.
// errors.Is/As still see err. Permanent(nil) is nil.
//
// Use it only when the failure is a property of the job itself (malformed or
// invalid payload). Configuration problems (a missing decryption key, an
// unregistered kind) and dependency outages must stay retryable: a rollout
// may fix them before the attempt budget runs out.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

func (e *PermanentError) Error() string { return permanentPrefix + e.Err.Error() }

func (e *PermanentError) Unwrap() error { return e.Err }

// IsPermanent reports whether err, or any error it wraps, is a PermanentError.
func IsPermanent(err error) bool {
	var permanent *PermanentError
	return errors.As(err, &permanent)
}
