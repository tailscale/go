// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Helpers shared by the h2interop Node.js client and server peers.

'use strict';

const ALPHABET = '0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ\n';
const HEADER_ALPHABET = 'abcdefghijklmnopqrstuvwxyz0123456789';

// maxChunk is the largest chunk patternChunk returns.
const maxChunk = 64 << 10;

// patternBuf is Pattern(maxChunk + 63), from which any chunk of
// Pattern can be sliced, as the pattern has period 63.
const patternBuf = (() => {
  const n = maxChunk + ALPHABET.length;
  const b = Buffer.alloc(n);
  for (let i = 0; i < n; i++) {
    b[i] = ALPHABET.charCodeAt(i % ALPHABET.length);
  }
  return b;
})();

// patternChunk returns bytes [off, off+len) of the body pattern.
// The len must be at most maxChunk.
function patternChunk(off, len) {
  if (len > maxChunk) {
    throw new Error('patternChunk: len too large');
  }
  const start = off % ALPHABET.length;
  return patternBuf.subarray(start, start + len);
}

// pattern returns Pattern(n) as a new Buffer.
function pattern(n) {
  const b = Buffer.alloc(n);
  for (let off = 0; off < n; off += maxChunk) {
    patternChunk(off, Math.min(maxChunk, n - off)).copy(b, off);
  }
  return b;
}

function headerPattern(n) {
  let s = '';
  for (let i = 0; i < n; i++) {
    s += HEADER_ALPHABET[i % HEADER_ALPHABET.length];
  }
  return s;
}

// Node's http2 module encodes and decodes header field values as
// latin1 (one JS char per byte). The JSON protocol uses Unicode
// strings whose wire form is UTF-8, so convert at the boundary.

// toWire converts a Unicode string to a latin1 "binary" string whose
// chars are its UTF-8 bytes.
function toWire(s) {
  return Buffer.from(String(s), 'utf8').toString('latin1');
}

// fromWire converts a latin1 header value from Node back to the
// Unicode string its bytes encode as UTF-8.
function fromWire(s) {
  return Buffer.from(String(s), 'latin1').toString('utf8');
}

module.exports = { patternChunk, pattern, headerPattern, toWire, fromWire, maxChunk };
