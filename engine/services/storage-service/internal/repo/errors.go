package repo

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ErrAlreadyExists is returned when a duplicate idempotency key is used with a
// different request body (03 §2: ALREADY_EXISTS code 9).
var ErrAlreadyExists = status.Error(codes.AlreadyExists, "idempotency key already used with different body")
