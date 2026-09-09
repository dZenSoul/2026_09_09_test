package domain

import "errors"

// Transport-independent error categories returned by application services.
// Their messages are intentionally generic and safe to expose through the API.
var (
	ErrInvalidArgument = errors.New("invalid argument")
	ErrUnauthorized    = errors.New("unauthorized")
	ErrForbidden       = errors.New("forbidden")
	ErrNotFound        = errors.New("not found")
	ErrNotImplemented  = errors.New("not implemented")
)
