// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This is the h2interop JSON client peer (see ../README.md) built on
// Node's node:http2 module. It reads a ClientSpec from stdin and
// writes a ClientOutput to stdout.

'use strict';

const http2 = require('node:http2');
const crypto = require('node:crypto');
const common = require('./common.js');

const { NGHTTP2_CANCEL, NGHTTP2_NO_ERROR, NGHTTP2_REFUSED_STREAM } = http2.constants;

// maxAttempts bounds retries of requests refused by the server.
const maxAttempts = 10;

// deadlineMS bounds the whole run, so that valid JSON is written
// before the runner's per-test timeout (60s by default) kills us.
const deadlineMS = 50000;

// maxReportedBody is the largest body reported in Result.body.
const maxReportedBody = 64 << 10;

const utf8Decoder = new TextDecoder('utf-8', { fatal: true });

function log(...args) {
  console.error('node client:', ...args);
}

// rawToFields converts a Node raw header list (alternating names and
// latin1-decoded values) to [name, value] pairs, skipping
// pseudo-header fields.
function rawToFields(raw) {
  const out = [];
  if (!raw) {
    return out;
  }
  for (let i = 0; i + 1 < raw.length; i += 2) {
    const name = String(raw[i]).toLowerCase();
    if (name.startsWith(':')) {
      continue;
    }
    out.push([name, common.fromWire(raw[i + 1])]);
  }
  return out;
}

function rawStatus(raw, headers) {
  if (raw) {
    for (let i = 0; i + 1 < raw.length; i += 2) {
      if (raw[i] === ':status') {
        return parseInt(raw[i + 1], 10);
      }
    }
  }
  return headers ? Number(headers[':status']) || 0 : 0;
}

// writeBody writes Pattern(n) to stream, respecting backpressure,
// and then ends the stream.
function writeBody(stream, n) {
  let off = 0;
  const step = () => {
    while (off < n) {
      if (stream.destroyed) {
        return;
      }
      const len = Math.min(16384, n - off);
      const chunk = common.patternChunk(off, len);
      off += len;
      if (!stream.write(chunk)) {
        stream.once('drain', step);
        return;
      }
    }
    if (!stream.destroyed) {
      stream.end();
    }
  };
  step();
}

// doRequest performs one request on session and resolves to its Result.
// It never rejects. A request refused by the server with
// RST_STREAM(REFUSED_STREAM) wasn't processed, so it is retried, as
// permitted by RFC 9113 Section 8.7.
async function doRequest(session, spec, rq, state) {
  let res;
  for (let attempt = 1; attempt <= maxAttempts; attempt++) {
    res = await doAttempt(session, spec, rq, state);
    if (!res.refused) {
      break;
    }
    log(`request ${rq.method} ${rq.path} refused (attempt ${attempt}); retrying`);
  }
  delete res.refused;
  return res;
}

// doAttempt makes one attempt at a request and resolves to its Result.
function doAttempt(session, spec, rq, state) {
  return new Promise((resolve) => {
    const res = {
      status: 0,
      proto: 'h2',
      header: [],
      body_len: 0,
    };
    const hash = crypto.createHash('sha256');
    const bodyChunks = [];
    let bodyKept = 0;
    let gotEnd = false;
    let done = false;

    const finish = () => {
      if (done) {
        return;
      }
      done = true;
      state.pending.delete(finish);
      res.body_sha256 = hash.digest('hex');
      if (res.body_len > 0 && res.body_len <= maxReportedBody) {
        try {
          res.body = utf8Decoder.decode(Buffer.concat(bodyChunks));
        } catch (e) {
          // Not valid UTF-8; don't report the body.
        }
      }
      resolve(res);
    };
    const fail = (msg) => {
      if (!res.error && !res.canceled) {
        res.error = msg;
      }
    };
    // Allow the deadline handler to force completion.
    state.pending.add(finish);
    finish.fail = fail;

    const url = new URL(spec.url);
    const authority = spec.authority || url.host;
    const bodyLen = rq.body_len || 0;
    const trailer = rq.trailer || [];
    const hasBody = bodyLen > 0 || trailer.length > 0;

    const raw = [
      ':method', rq.method || 'GET',
      ':path', rq.path,
      ':scheme', url.protocol.replace(/:$/, ''),
      ':authority', authority,
    ];
    for (const [k, v] of rq.header || []) {
      raw.push(k, common.toWire(v));
    }
    if (bodyLen > 0 && !rq.no_content_length) {
      raw.push('content-length', String(bodyLen));
    }
    if (rq.expect_continue) {
      raw.push('expect', '100-continue');
    }

    let stream;
    try {
      stream = session.request(raw, {
        endStream: !hasBody,
        waitForTrailers: trailer.length > 0,
      });
    } catch (e) {
      fail('request: ' + e.message);
      finish();
      return;
    }

    let bodyStarted = false;
    const startBody = () => {
      if (bodyStarted || !hasBody) {
        return;
      }
      bodyStarted = true;
      writeBody(stream, bodyLen);
    };

    if (trailer.length > 0) {
      stream.on('wantTrailers', () => {
        const t = {};
        for (const [k, v] of trailer) {
          (t[k] = t[k] || []).push(common.toWire(v));
        }
        try {
          stream.sendTrailers(t);
        } catch (e) {
          fail('sendTrailers: ' + e.message);
        }
      });
    }

    stream.on('continue', () => startBody());
    stream.on('headers', (headers, flags, rawh) => {
      const st = rawStatus(rawh, headers);
      res.informational = res.informational || [];
      res.informational.push({ status: st, header: rawToFields(rawh) });
    });
    stream.on('response', (headers, flags, rawh) => {
      res.status = rawStatus(rawh, headers);
      res.header = rawToFields(rawh);
      // If the server sent a final response without a 100 Continue,
      // send the body anyway, as we promised it in the headers.
      startBody();
    });
    stream.on('trailers', (headers, flags, rawh) => {
      res.trailer = rawToFields(rawh);
    });
    stream.on('data', (chunk) => {
      res.body_len += chunk.length;
      hash.update(chunk);
      if (bodyKept <= maxReportedBody) {
        bodyChunks.push(chunk);
        bodyKept += chunk.length;
      }
      if (rq.cancel_after > 0 && !res.canceled && res.body_len >= rq.cancel_after) {
        res.canceled = true;
        stream.close(NGHTTP2_CANCEL);
      }
    });
    stream.on('end', () => {
      gotEnd = true;
    });
    stream.on('aborted', () => {
      fail('stream aborted');
    });
    stream.on('error', (e) => {
      fail((e.code ? e.code + ': ' : '') + e.message);
    });
    stream.on('close', () => {
      const code = stream.rstCode;
      if (code === NGHTTP2_REFUSED_STREAM && res.status === 0 && !state.sessionError) {
        res.refused = true;
      }
      if (code !== undefined && code !== NGHTTP2_NO_ERROR) {
        fail('stream closed with RST_STREAM error code ' + code);
      } else if (!gotEnd && !res.canceled) {
        fail('stream closed before end of response');
      }
      if (state.sessionError && !gotEnd) {
        fail('session error: ' + state.sessionError);
      }
      finish();
    });

    if (hasBody && !rq.expect_continue) {
      startBody();
    }
  });
}

async function run(spec) {
  const out = { results: [] };
  const url = new URL(spec.url);
  const opts = {};
  if (url.protocol === 'https:') {
    opts.rejectUnauthorized = false;
    opts.ALPNProtocols = ['h2'];
  }
  const state = { pending: new Set(), sessionError: null };
  const session = http2.connect(url.origin, opts);
  session.on('error', (e) => {
    state.sessionError = (e.code ? e.code + ': ' : '') + e.message;
    log('session error:', e);
  });
  session.on('goaway', (code, lastStreamID) => {
    log('received GOAWAY code', code, 'last stream', lastStreamID);
  });
  session.on('frameError', (type, code, id) => {
    log('frame error type', type, 'code', code, 'stream', id);
  });

  const deadline = setTimeout(() => {
    log('deadline exceeded; abandoning', state.pending.size, 'requests');
    for (const f of [...state.pending]) {
      f.fail('client deadline exceeded');
      f();
    }
  }, deadlineMS);

  const reqs = spec.requests || [];
  if (spec.concurrent) {
    out.results = await Promise.all(reqs.map((rq) => doRequest(session, spec, rq, state)));
  } else {
    for (const rq of reqs) {
      out.results.push(await doRequest(session, spec, rq, state));
    }
  }
  clearTimeout(deadline);
  if (reqs.length > 0 && state.sessionError && out.results.every((r) => r.error)) {
    out.error = 'session error: ' + state.sessionError;
  }
  session.close();
  return out;
}

function emit(out) {
  process.stdout.write(JSON.stringify(out) + '\n', () => process.exit(0));
}

function main() {
  const chunks = [];
  process.stdin.on('data', (c) => chunks.push(c));
  process.stdin.on('end', () => {
    let spec;
    try {
      spec = JSON.parse(Buffer.concat(chunks).toString('utf8'));
    } catch (e) {
      emit({ results: [], error: 'bad spec: ' + e.message });
      return;
    }
    run(spec).then(emit, (e) => {
      log('fatal:', e);
      emit({ results: [], error: 'fatal: ' + (e && e.stack || e) });
    });
  });
}

// An exception in one request's event handlers shouldn't take down
// the others; the deadline ensures we still finish.
process.on('uncaughtException', (e) => {
  log('uncaught exception:', e);
});

main();
