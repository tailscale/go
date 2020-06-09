// +build omitidna

package idna

import "errors"

var errDisabled = errors.New("idna: support for IDNA is compiled out")

func ToASCII(s string) (string, error) {
	if isASCII(s) {
		return s, nil
	}
	return "", errDisabled
}

func isASCII(s string) bool {
	const RuneSelf = 0x80
	for i := 0; i < len(s); i++ {
		if s[0] >= RuneSelf {
			return false
		}
	}
	return true
}

var Lookup fakeProfile

type fakeProfile struct{}

func (fakeProfile) ToASCII(s string) (string, error) {
	return ToASCII(s)
}
