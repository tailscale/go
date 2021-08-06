// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package unicode provides data and functions to test some properties of
// Unicode code points.
package unicode

import (
	"internal/unicodelite"
)

const (
	MaxRune         = '\U0010FFFF' // Maximum valid Unicode code point.
	ReplacementChar = '\uFFFD'     // Represents invalid code points.
	MaxASCII        = '\u007F'     // maximum ASCII value.
	MaxLatin1       = '\u00FF'     // maximum Latin-1 value.
)

// RangeTable defines a set of Unicode code points by listing the ranges of
// code points within the set. The ranges are listed in two slices
// to save space: a slice of 16-bit ranges and a slice of 32-bit ranges.
// The two slices must be in sorted order and non-overlapping.
// Also, R32 should contain only values >= 0x10000 (1<<16).
type RangeTable = unicodelite.RangeTable

// Range16 represents of a range of 16-bit Unicode code points. The range runs from Lo to Hi
// inclusive and has the specified stride.
type Range16 = unicodelite.Range16

// Range32 represents of a range of Unicode code points and is used when one or
// more of the values will not fit in 16 bits. The range runs from Lo to Hi
// inclusive and has the specified stride. Lo and Hi must always be >= 1<<16.
type Range32 = unicodelite.Range32

// CaseRange represents a range of Unicode code points for simple (one
// code point to one code point) case conversion.
// The range runs from Lo to Hi inclusive, with a fixed stride of 1. Deltas
// are the number to add to the code point to reach the code point for a
// different case for that character. They may be negative. If zero, it
// means the character is in the corresponding case. There is a special
// case representing sequences of alternating corresponding Upper and Lower
// pairs. It appears with a fixed Delta of
//	{UpperLower, UpperLower, UpperLower}
// The constant UpperLower has an otherwise impossible delta value.
type CaseRange = unicodelite.CaseRange

// SpecialCase represents language-specific case mappings such as Turkish.
// Methods of SpecialCase customize (by overriding) the standard mappings.
type SpecialCase = unicodelite.SpecialCase

// BUG(r): There is no mechanism for full case folding, that is, for
// characters that involve multiple runes in the input or output.

// Indices into the Delta arrays inside CaseRanges for case mapping.
const (
	UpperCase = iota
	LowerCase
	TitleCase
	MaxCase
)

type d = unicodelite.D

// If the Delta field of a CaseRange is UpperLower, it means
// this CaseRange represents a sequence of the form (say)
// Upper Lower Upper Lower.
const (
	UpperLower = MaxRune + 1 // (Cannot be a valid delta.)
)

// Is reports whether the rune is in the specified table of ranges.
func Is(rangeTab *RangeTable, r rune) bool {
	return unicodelite.Is(rangeTab, r)
}

// IsUpper reports whether the rune is an upper case letter.
func IsUpper(r rune) bool {
	return unicodelite.IsUpper(r)
}

// IsLower reports whether the rune is a lower case letter.
func IsLower(r rune) bool {
	return unicodelite.IsLower(r)
}

// IsTitle reports whether the rune is a title case letter.
func IsTitle(r rune) bool {
	return unicodelite.IsTitle(r)
}

// To maps the rune to the specified case: UpperCase, LowerCase, or TitleCase.
func To(_case int, r rune) rune {
	return unicodelite.To(_case, r)
}

// ToUpper maps the rune to upper case.
func ToUpper(r rune) rune {
	return unicodelite.ToUpper(r)
}

// ToLower maps the rune to lower case.
func ToLower(r rune) rune {
	return unicodelite.ToLower(r)
}

// ToTitle maps the rune to title case.
func ToTitle(r rune) rune {
	return unicodelite.ToTitle(r)
}

// caseOrbit is defined in tables.go as []foldPair. Right now all the
// entries fit in uint16, so use uint16. If that changes, compilation
// will fail (the constants in the composite literal will not fit in uint16)
// and the types here can change to uint32.
type foldPair struct {
	From uint16
	To   uint16
}

// SimpleFold iterates over Unicode code points equivalent under
// the Unicode-defined simple case folding. Among the code points
// equivalent to rune (including rune itself), SimpleFold returns the
// smallest rune > r if one exists, or else the smallest rune >= 0.
// If r is not a valid Unicode code point, SimpleFold(r) returns r.
//
// For example:
//	SimpleFold('A') = 'a'
//	SimpleFold('a') = 'A'
//
//	SimpleFold('K') = 'k'
//	SimpleFold('k') = '\u212A' (Kelvin symbol, K)
//	SimpleFold('\u212A') = 'K'
//
//	SimpleFold('1') = '1'
//
//	SimpleFold(-2) = -2
//
func SimpleFold(r rune) rune {
	return unicodelite.SimpleFold(r)
}
