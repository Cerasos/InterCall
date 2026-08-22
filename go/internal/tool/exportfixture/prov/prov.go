// Package prov is the provider fixture of the export-binding tests in
// internal/tool: tagged providers, reachable ordinary types, and
// application exceptions. The checked-in export binding in the parent
// package (binding_gen.go) is generated from this package through the
// complete discovery, model, and emission pipeline.
package prov

import (
	"context"
	"errors"
	"fmt"

	intercall "github.com/cerasos/intercall/go"
)

// ErrDenied is the denied sentinel.
// @intercall exception denied
var ErrDenied = errors.New("denied")

// ErrShared is a payload-typed sentinel: its value is also a payload
// exception value, so a provider returning it produces two direct
// matches and the dispatch selects internal_exception.
// @intercall exception err_shared
var ErrShared error = &Shared{Code: 3}

// ErrWeird is an uncomparable sentinel: comparing a provider error of
// the same dynamic type panics during matching, and the runtime's one
// recovery around the complete dispatch sends internal_exception.
// @intercall exception err_weird
var ErrWeird error = Weird{Tags: []string{"sentinel"}}

// Failed carries the failure details.
// @intercall exception failed
type Failed struct {
	// The failure code.
	Code    int32
	Message string
}

// Error implements error for Failed.
func (f *Failed) Error() string { return "failed" }

// Shared is a payload exception.
// @intercall exception shared
type Shared struct {
	// The shared code.
	Code int32
}

// Error implements error for Shared.
func (s *Shared) Error() string { return "shared" }

// Empty is a zero-field payload exception: its payload record carries
// no wire bytes.
// @intercall exception empty
type Empty struct{}

// Error implements error for Empty.
func (e *Empty) Error() string { return "empty" }

// Weird is an uncomparable error value type used only as the ErrWeird
// sentinel value; it is never a wire value.
type Weird struct {
	Tags []string
}

// Error implements error for Weird.
func (w Weird) Error() string { return "weird" }

// UserID is a user identifier.
// @intercall type user_id
type UserID uint64

// Point is a geometric point.
// @intercall type point
type Point struct {
	// Horizontal coordinate.
	X float64
	// Vertical coordinate.
	Y float64
}

// Echo echoes its input.
//
// The mode strings drive the direct-matching fallback paths of the
// generated dispatch: "denied" returns the denied sentinel, "failed"
// a failed payload, "shared" the shared sentinel (two direct matches),
// "typed_nil" a typed-nil payload pointer, "wrapped" a wrapped error
// that no direct comparison matches, "weird" an uncomparable error
// that panics during matching, "empty" a zero-field payload, and
// "bad_utf8" a success value the encoder rejects.
// @intercall procedure echo
// @param value The value to echo.
// @return The unchanged input.
func Echo(ctx context.Context, value string) (string, error) {
	switch value {
	case "invalid_arguments":
		return "", intercall.ErrInvalidArguments
	case "procedure_not_found":
		return "", intercall.ErrProcedureNotFound
	case "internal_exception":
		return "", intercall.ErrInternalException
	case "denied":
		return "", ErrDenied
	case "failed":
		return "", &Failed{Code: 7, Message: "boom"}
	case "shared":
		return "", ErrShared
	case "typed_nil":
		var s *Shared
		return "", s
	case "wrapped":
		return "", fmt.Errorf("wrapped: %w", ErrDenied)
	case "bad_utf8":
		return "bad\xff", nil
	case "weird":
		return "", Weird{Tags: []string{"value"}}
	case "empty":
		return "", &Empty{}
	}
	return value, nil
}

// Add adds two integers.
// @intercall procedure add
func Add(ctx context.Context, a int64, b int64) (int64, error) { return a + b, nil }

// Crash panics on demand; the runtime recovery sends internal_exception.
// @intercall procedure crash
// @param mode The crash mode; "panic" panics.
func Crash(ctx context.Context, mode string) error {
	if mode == "panic" {
		panic("provider panic")
	}
	return nil
}

// Sample echoes its byte payload.
// @intercall procedure sample
func Sample(ctx context.Context, data []byte, channel uint8) ([]byte, error) {
	return append([]byte(nil), data...), nil
}

// Wave counts its samples.
// @intercall procedure wave
func Wave(ctx context.Context, samples []uint8) (uint32, error) {
	return uint32(len(samples)), nil
}

// Paint paints a rectangle and returns its size.
// @intercall procedure paint
// @param origin The rectangle origin.
// @param size The rectangle size.
// @return The painted size.
func Paint(ctx context.Context, origin struct {
	X int32
	Y int32
}, size struct {
	Width  uint32
	Height uint32
}) (struct {
	Width  uint32
	Height uint32
}, error) {
	return size, nil
}

// Fetch returns the point of one user.
// @intercall procedure fetch
// @param id The user id.
// @return The user's point.
func Fetch(ctx context.Context, id UserID) (Point, error) {
	return Point{X: float64(id), Y: float64(id + 1)}, nil
}

// Ping checks liveness.
// @intercall procedure ping
func Ping(ctx context.Context) error { return nil }
