// +build !x509omitsha1

package x509

import (
	_ "crypto/sha1"
)
