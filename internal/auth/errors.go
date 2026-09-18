package auth

import "errors"

var (
	ErrNoKey       = errors.New("missing API key")
	ErrInvalidKey  = errors.New("invalid API key")
	ErrKeyDisabled = errors.New("API key disabled")
)
