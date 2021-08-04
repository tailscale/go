// +build noasm !amd64,!arm64

package elliptic

import (
	"math/big"
)

type curve struct{ Curve }

// P384 returns a Curve which implements NIST P-384 (FIPS 186-3, section D.2.4),
// also known as secp384r1. The CurveParams.Name of this Curve is "P-384".
//
// Multiple invocations of this function will return the same value, so it can
// be used for equality checks and switch statements.
//
// The cryptographic operations do not use constant-time algorithms.
func P384() Curve {
	initonce.Do(initAll)
	return p384
}

// CombinedMult calculates P=mG+nQ, where G is the generator and Q=(x,y,z).
// The scalars m and n are integers in big-endian form. Non-constant time.
func (c curve) CombinedMult(xQ, yQ *big.Int, m, n []byte) (xP, yP *big.Int) {
	x1, y1 := c.ScalarBaseMult(m)
	x2, y2 := c.ScalarMult(xQ, yQ, n)
	return c.Add(x1, y1, x2, y2)
}
