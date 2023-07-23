package http

var roundTripEnforcer func(*Request) error

// SetRoundTripEnforcer set a program-global resolver enforcer that can cause
// RoundTrip calls to fail based on the request and its context.
//
// f must be non-nil.
//
// SetRoundTripEnforcer can only be called once, and must not be called
// concurrent with any RoundTrip call; it's expected to be registered during
// init.
func SetRoundTripEnforcer(f func(*Request) error) {
	if f == nil {
		panic("nil func")
	}
	if roundTripEnforcer != nil {
		panic("already called")
	}
	roundTripEnforcer = f
}
