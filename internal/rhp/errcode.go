package rhp

import "fmt"

// RHPv2 errCode values (docs/rhp2-server.md §RhpErrorCode). Codes 0–16 are the
// published spec; 17 ("Not connected") is XRouter-observed.
const (
	ErrOk                   = 0
	ErrUnspecified          = 1
	ErrBadOrMissingType     = 2
	ErrInvalidHandle        = 3
	ErrNoMemory             = 4
	ErrBadOrMissingMode     = 5
	ErrInvalidLocalAddress  = 6
	ErrInvalidRemoteAddress = 7
	ErrBadOrMissingFamily   = 8
	ErrDuplicateSocket      = 9
	ErrNoSuchPort           = 10
	ErrInvalidProtocol      = 11
	ErrBadParameter         = 12
	ErrNoBuffers            = 13
	ErrUnauthorised         = 14
	ErrNoRoute              = 15
	ErrOpNotSupported       = 16
	ErrNotConnected         = 17
)

var errText = map[int]string{
	ErrOk:                   "Ok",
	ErrUnspecified:          "Unspecified",
	ErrBadOrMissingType:     "Bad or missing type",
	ErrInvalidHandle:        "Invalid handle",
	ErrNoMemory:             "No memory",
	ErrBadOrMissingMode:     "Bad or missing mode",
	ErrInvalidLocalAddress:  "Invalid local address",
	ErrInvalidRemoteAddress: "Invalid remote address",
	ErrBadOrMissingFamily:   "Bad or missing family",
	ErrDuplicateSocket:      "Duplicate socket",
	ErrNoSuchPort:           "No such port",
	ErrInvalidProtocol:      "Invalid protocol",
	ErrBadParameter:         "Bad parameter",
	ErrNoBuffers:            "No buffers",
	ErrUnauthorised:         "Unauthorised",
	ErrNoRoute:              "No Route",
	ErrOpNotSupported:       "Operation not supported",
	ErrNotConnected:         "Not connected",
}

// ErrText returns the canonical errText for a code (matching the spec's own
// inconsistent capitalisation), or a placeholder for an unknown future code.
func ErrText(code int) string {
	if t, ok := errText[code]; ok {
		return t
	}
	return fmt.Sprintf("Unknown (%d)", code)
}

// ServerError is a non-zero errCode returned by the node in reply to a
// request.
type ServerError struct {
	Code int
	Text string
}

func (e *ServerError) Error() string {
	t := e.Text
	if t == "" {
		t = ErrText(e.Code)
	}
	return fmt.Sprintf("rhp: errCode %d (%s)", e.Code, t)
}

// IsCallsignInUse reports whether a bind refusal means the callsign is already
// claimed — the trigger for an SSID probe-walk (mirrors pdn-convers's
// RhpNodeLink): the canonical duplicate-bind code, plus the in-use-local-address
// code some nodes report instead.
func IsCallsignInUse(code int) bool {
	return code == ErrDuplicateSocket || code == ErrInvalidLocalAddress
}
