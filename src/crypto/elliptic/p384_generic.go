// +build noasm !amd64,!arm64

package elliptic

import (
	"math/big"
)

type curve struct{ Curve }

func P384() Curve { return curve{P384()} }

// CombinedMult calculates P=mG+nQ, where G is the generator and Q=(x,y,z).
// The scalars m and n are integers in big-endian form. Non-constant time.
func (c curve) CombinedMult(xQ, yQ *big.Int, m, n []byte) (xP, yP *big.Int) {
	x1, y1 := c.ScalarBaseMult(m)
	x2, y2 := c.ScalarMult(xQ, yQ, n)
	return c.Add(x1, y1, x2, y2)
}
