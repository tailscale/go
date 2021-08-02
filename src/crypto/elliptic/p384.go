package elliptic

import (
	"math/big"
)

// Params returns the parameters for the curve. Note: The value returned by
// this function fallbacks to the stdlib implementation of elliptic curve
// operations. Use this method to only recover elliptic curve parameters.
func (c curve) Params() *CurveParams {
	initonce.Do(initAll)
	return p384.Params()
}

// IsAtInfinity returns True is the point is the identity point.
func (c curve) IsAtInfinity(x, y *big.Int) bool {
	return x.Sign() == 0 && y.Sign() == 0
}
