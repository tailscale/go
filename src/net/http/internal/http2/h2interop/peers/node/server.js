// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This is the h2interop JSON server peer (see ../README.md) built on
// Node's node:http2 module. It is run as "server '<ServerSpec JSON>'"
// and serves the route set until killed.

'use strict';

const http2 = require('node:http2');
const crypto = require('node:crypto');
const fs = require('node:fs');
const common = require('./common.js');

const { NGHTTP2_INTERNAL_ERROR } = http2.constants;

const helloBody = 'Hello, HTTP/2 interop!\n';
const trailersBody = 'trailers follow\n';

function log(...args) {
  console.error('node server:', ...args);
}

// settingsOptions maps RFC setting names to Node's settings object keys.
const settingsOptions = {
  SETTINGS_HEADER_TABLE_SIZE: 'headerTableSize',
  SETTINGS_ENABLE_PUSH: 'enablePush',
  SETTINGS_MAX_CONCURRENT_STREAMS: 'maxConcurrentStreams',
  SETTINGS_INITIAL_WINDOW_SIZE: 'initialWindowSize',
  SETTINGS_MAX_FRAME_SIZE: 'maxFrameSize',
  SETTINGS_MAX_HEADER_LIST_SIZE: 'maxHeaderListSize',
  SETTINGS_ENABLE_CONNECT_PROTOCOL: 'enableConnectProtocol',
};

function nodeSettings(m) {
  const s = {};
  for (const [k, v] of Object.entries(m || {})) {
    const opt = settingsOptions[k];
    if (!opt) {
      log('ignoring unsupported setting', k);
      continue;
    }
    if (opt === 'enablePush' || opt === 'enableConnectProtocol') {
      s[opt] = v !== 0;
    } else {
      s[opt] = v;
    }
  }
  return s;
}

// respond sends a complete response with an optional body.
function respond(stream, status, headers, body) {
  if (stream.destroyed || stream.headersSent) {
    return;
  }
  const h = Object.assign({ ':status': status }, headers);
  const noBody = body === undefined || body.length === 0 || stream.headRequest;
  // Node ends HEAD responses after the headers itself.
  stream.respond(h, { endStream: noBody });
  if (!noBody) {
    stream.end(body);
  }
}

function respondJSON(stream, v) {
  const j = Buffer.from(JSON.stringify(v), 'utf8');
  respond(stream, 200, { 'content-type': 'application/json', 'content-length': j.length }, j);
}

// writeAsync writes chunk to stream and resolves once Node has handed
// it to nghttp2, so that each chunk becomes its own DATA frame.
function writeAsync(stream, chunk) {
  return new Promise((resolve, reject) => {
    stream.write(chunk, (err) => (err ? reject(err) : resolve()));
  });
}

// rawFields converts a Node raw header list to [name, value] pairs
// in the order received, excluding pseudo-header fields.
function rawFields(raw) {
  const out = [];
  for (let i = 0; i + 1 < raw.length; i += 2) {
    const name = String(raw[i]).toLowerCase();
    if (!name.startsWith(':')) {
      out.push([name, common.fromWire(raw[i + 1])]);
    }
  }
  return out;
}

function fieldsMap(fields) {
  const m = {};
  for (const [k, v] of fields) {
    (m[k] = m[k] || []).push(v);
  }
  return m;
}

function intParam(stream, s) {
  if (!/^[0-9]+$/.test(s)) {
    respond(stream, 400, { 'content-type': 'text/plain' }, 'bad parameter\n');
    return -1;
  }
  return parseInt(s, 10);
}

const routes = {
  hello(stream) {
    respond(stream, 200, { 'content-type': 'text/plain', 'content-length': helloBody.length }, helloBody);
  },

  bytes(stream, arg) {
    const n = intParam(stream, arg);
    if (n < 0) {
      return;
    }
    respond(stream, 200, { 'content-type': 'application/octet-stream', 'content-length': n },
      common.pattern(n));
  },

  async stream(stream, arg) {
    const n = intParam(stream, arg);
    if (n < 0) {
      return;
    }
    stream.respond({ ':status': 200, 'content-type': 'application/octet-stream' },
      { endStream: stream.headRequest });
    if (stream.headRequest) {
      return;
    }
    for (let off = 0; off < n; off += 1000) {
      if (stream.destroyed) {
        return;
      }
      await writeAsync(stream, common.patternChunk(off, Math.min(1000, n - off)));
    }
    stream.end();
  },

  echo(stream, arg, headers) {
    const h = { ':status': 200, 'content-type': 'application/octet-stream' };
    const cl = headers['content-length'];
    if (cl !== undefined) {
      h['content-length'] = cl;
    }
    stream.respond(h, { endStream: stream.headRequest });
    if (stream.headRequest) {
      stream.resume();
      return;
    }
    stream.on('data', (chunk) => {
      if (!stream.write(chunk)) {
        stream.pause();
        stream.once('drain', () => stream.resume());
      }
    });
    stream.on('end', () => stream.end());
  },

  upload(stream, arg, headers) {
    const hash = crypto.createHash('sha256');
    let n = 0;
    let trailer;
    stream.on('trailers', (th, flags, raw) => {
      trailer = fieldsMap(rawFields(raw || []));
    });
    stream.on('data', (chunk) => {
      n += chunk.length;
      hash.update(chunk);
    });
    stream.on('end', () => {
      const cl = headers['content-length'];
      const ub = {
        len: n,
        sha256: hash.digest('hex'),
        content_length: cl !== undefined && /^[0-9]+$/.test(cl) ? parseInt(cl, 10) : -1,
      };
      if (trailer && Object.keys(trailer).length > 0) {
        ub.trailer = trailer;
      }
      respondJSON(stream, ub);
    });
  },

  trailers(stream) {
    stream.respond({ ':status': 200, 'content-type': 'text/plain' },
      { waitForTrailers: !stream.headRequest, endStream: stream.headRequest });
    if (stream.headRequest) {
      return;
    }
    stream.on('wantTrailers', () => {
      stream.sendTrailers({ 'x-trailer-a': '1', 'x-trailer-b': 'two' });
    });
    stream.end(trailersBody);
  },

  status(stream, arg) {
    const code = intParam(stream, arg);
    if (code < 0) {
      return;
    }
    respond(stream, code, {});
  },

  bigheader(stream, arg) {
    const n = intParam(stream, arg);
    if (n < 0) {
      return;
    }
    respond(stream, 200, { 'x-big': common.headerPattern(n) }, 'ok\n');
  },

  manyheaders(stream, arg) {
    const n = intParam(stream, arg);
    if (n < 0) {
      return;
    }
    const h = {};
    for (let i = 0; i < n; i++) {
      h['x-h-' + i] = 'value-' + i;
    }
    respond(stream, 200, h, 'ok\n');
  },

  'early-hints'(stream) {
    stream.additionalHeaders({ ':status': 103, link: '</style.css>; rel=preload; as=style' });
    respond(stream, 200, {}, 'ok\n');
  },

  info(stream, arg, headers, raw) {
    respondJSON(stream, {
      method: headers[':method'],
      path: headers[':path'],
      authority: headers[':authority'] !== undefined ? headers[':authority'] : (headers.host || ''),
      proto: 'HTTP/2.0',
      header: fieldsMap(rawFields(raw)),
    });
  },

  delay(stream, arg) {
    const ms = intParam(stream, arg);
    if (ms < 0) {
      return;
    }
    const t = setTimeout(() => respond(stream, 200, {}, 'ok\n'), ms);
    stream.on('close', () => clearTimeout(t));
  },

  async rst(stream) {
    stream.respond({ ':status': 200, 'content-type': 'application/octet-stream' });
    await writeAsync(stream, common.pattern(1000));
    // Node's stream.close(code) first ends the writable side when
    // it's still open, so nghttp2 sends DATA with END_STREAM and then
    // drops the RST_STREAM for the now-closed stream. Destroying the
    // stream with an error resets it with INTERNAL_ERROR without
    // ending it first.
    stream.destroy(new Error('intentional reset by /rst route'));
  },
};

function onStream(stream, headers, flags, raw) {
  stream.on('error', (e) => log('stream error:', e.code || '', e.message));
  const path = headers[':path'] || '';
  const p = path.split('?')[0];
  const m = /^\/([^/]+)(?:\/([^/]*))?$/.exec(p);
  const route = m && Object.prototype.hasOwnProperty.call(routes, m[1]) ? routes[m[1]] : null;
  const takesArg = m && ['bytes', 'stream', 'status', 'bigheader', 'manyheaders', 'delay'].includes(m[1]);
  if (!route || takesArg !== (m[2] !== undefined)) {
    stream.resume();
    respond(stream, 404, { 'content-type': 'text/plain' }, 'not found\n');
    return;
  }
  // Like Node's compatibility API (and Go's server), tell clients
  // that are waiting for permission to send the request body to go
  // ahead.
  if (String(headers.expect || '').toLowerCase() === '100-continue') {
    stream.additionalHeaders({ ':status': 100 });
  }
  // Routes that consume the request body attach their own listeners;
  // for the rest, discard it so flow control windows are replenished.
  if (!['echo', 'upload'].includes(m[1])) {
    stream.resume();
  }
  try {
    const r = route(stream, m[2], headers, raw || []);
    if (r && typeof r.catch === 'function') {
      r.catch((e) => log('handler', p, 'error:', e.code || '', e.message));
    }
  } catch (e) {
    log('handler', p, 'threw:', e);
    if (!stream.destroyed) {
      stream.close(NGHTTP2_INTERNAL_ERROR);
    }
  }
}

function main() {
  const spec = JSON.parse(process.argv[2]);
  const opts = {
    settings: nodeSettings(spec.settings),
  };
  let server;
  if (spec.tls) {
    opts.allowHTTP1 = false;
    opts.ALPNProtocols = ['h2'];
    opts.cert = fs.readFileSync(spec.cert);
    opts.key = fs.readFileSync(spec.key);
    server = http2.createSecureServer(opts);
    server.on('tlsClientError', (e) => log('TLS client error:', e.code || '', e.message));
  } else {
    server = http2.createServer(opts);
  }
  server.on('stream', onStream);
  server.on('sessionError', (e) => log('session error:', e.code || '', e.message));
  server.on('session', (session) => {
    session.on('error', (e) => log('session error:', e.code || '', e.message));
    session.on('frameError', (type, code, id) => log('frame error type', type, 'code', code, 'stream', id));
    session.on('goaway', (code, last) => log('received GOAWAY code', code, 'last stream', last));
  });
  server.on('error', (e) => log('server error:', e));
  const i = spec.addr.lastIndexOf(':');
  const host = spec.addr.slice(0, i);
  const port = parseInt(spec.addr.slice(i + 1), 10);
  server.listen(port, host, () => {
    log('listening on', spec.addr, spec.tls ? '(TLS)' : '(h2c)', 'settings', JSON.stringify(opts.settings));
  });
}

process.on('uncaughtException', (e) => log('uncaught exception:', e));
process.on('unhandledRejection', (e) => log('unhandled rejection:', e));

main();
