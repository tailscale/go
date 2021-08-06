// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// TODO: This file contains the special casing rules for Turkish and Azeri only.
// It should encompass all the languages with special casing rules
// and be generated automatically, but that requires some API
// development first.

package unicodelite

var TurkishCase SpecialCase = _TurkishCase
var _TurkishCase = SpecialCase{
	CaseRange{0x0049, 0x0049, D{0, 0x131 - 0x49, 0}},
	CaseRange{0x0069, 0x0069, D{0x130 - 0x69, 0, 0x130 - 0x69}},
	CaseRange{0x0130, 0x0130, D{0, 0x69 - 0x130, 0}},
	CaseRange{0x0131, 0x0131, D{0x49 - 0x131, 0, 0x49 - 0x131}},
}

var AzeriCase SpecialCase = _TurkishCase
