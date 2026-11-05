package engine

import (
	"errors"
	"fmt"
)

// Kind classifies an error the way the Docker Engine API reports it.
type Kind int

const (
	KindInternal Kind = iota
	KindInvalid
	KindNotFound
	KindConflict
	KindNotModified
	KindUnsupported
	KindUnauthorized
	KindForbidden
)

type Error struct {
	Kind Kind
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

func errorf(k Kind, format string, a ...any) error {
	return &Error{Kind: k, Msg: fmt.Sprintf(format, a...)}
}

func invalid(format string, a ...any) error     { return errorf(KindInvalid, format, a...) }
func notFound(format string, a ...any) error    { return errorf(KindNotFound, format, a...) }
func conflict(format string, a ...any) error    { return errorf(KindConflict, format, a...) }
func notModified(format string, a ...any) error { return errorf(KindNotModified, format, a...) }
func unsupported(format string, a ...any) error { return errorf(KindUnsupported, format, a...) }
func forbidden(format string, a ...any) error   { return errorf(KindForbidden, format, a...) }
func unauthorized(format string, a ...any) error {
	return errorf(KindUnauthorized, format, a...)
}

// KindOf returns the error's kind, KindInternal for errors the engine did not
// classify.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindInternal
}
