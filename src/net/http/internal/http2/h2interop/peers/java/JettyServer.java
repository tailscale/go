// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.nio.ByteBuffer;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.security.KeyFactory;
import java.security.KeyStore;
import java.security.MessageDigest;
import java.security.PrivateKey;
import java.security.cert.Certificate;
import java.security.cert.CertificateFactory;
import java.security.spec.PKCS8EncodedKeySpec;
import java.util.ArrayList;
import java.util.Base64;
import java.util.HexFormat;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.CountDownLatch;
import org.eclipse.jetty.alpn.server.ALPNServerConnectionFactory;
import org.eclipse.jetty.http.HttpField;
import org.eclipse.jetty.http.HttpFields;
import org.eclipse.jetty.http.HttpURI;
import org.eclipse.jetty.http.Trailers;
import org.eclipse.jetty.http2.ErrorCode;
import org.eclipse.jetty.http2.HTTP2Cipher;
import org.eclipse.jetty.http2.HTTP2Connection;
import org.eclipse.jetty.http2.api.Stream;
import org.eclipse.jetty.http2.frames.ResetFrame;
import org.eclipse.jetty.http2.server.AbstractHTTP2ServerConnectionFactory;
import org.eclipse.jetty.http2.server.HTTP2CServerConnectionFactory;
import org.eclipse.jetty.http2.server.HTTP2ServerConnectionFactory;
import org.eclipse.jetty.io.Content;
import org.eclipse.jetty.server.Handler;
import org.eclipse.jetty.server.HttpConfiguration;
import org.eclipse.jetty.server.Request;
import org.eclipse.jetty.server.Response;
import org.eclipse.jetty.server.SecureRequestCustomizer;
import org.eclipse.jetty.server.Server;
import org.eclipse.jetty.server.ServerConnector;
import org.eclipse.jetty.server.SslConnectionFactory;
import org.eclipse.jetty.util.Callback;
import org.eclipse.jetty.util.ssl.SslContextFactory;
import org.eclipse.jetty.util.thread.QueuedThreadPool;

// JettyServer is the h2interop peer server using Jetty's HTTP/2 server.
// It's run with a ServerSpec JSON document as its argument and serves
// the route set described in ../README.md until killed.
public final class JettyServer {
    private static final String ALPHABET =
            "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ\n";
    private static final String HEADER_ALPHABET = "abcdefghijklmnopqrstuvwxyz0123456789";
    private static final String HELLO = "Hello, HTTP/2 interop!\n";

    // headerSize is the maximum request and response header size.
    // Jetty's default of 8KiB is too small for the bigheader tests.
    private static final int HEADER_SIZE = 1 << 20;

    @SuppressWarnings("unchecked")
    public static void main(String[] args) throws Exception {
        if (args.length != 1) {
            System.err.println("usage: server '<ServerSpec JSON>'");
            System.exit(2);
        }
        Map<String, Object> spec = (Map<String, Object>) Json.parse(args[0]);
        String addr = (String) spec.get("addr");
        int colon = addr.lastIndexOf(':');
        String host = addr.substring(0, colon);
        int port = Integer.parseInt(addr.substring(colon + 1));
        boolean tls = Boolean.TRUE.equals(spec.get("tls"));
        Map<String, Object> settings = (Map<String, Object>) spec.get("settings");
        if (settings == null) {
            settings = Map.of();
        }

        QueuedThreadPool pool = new QueuedThreadPool(500);
        pool.setName("jetty");
        Server server = new Server(pool);

        HttpConfiguration cfg = new HttpConfiguration();
        cfg.setSendServerVersion(false);
        cfg.setRequestHeaderSize(HEADER_SIZE);
        cfg.setResponseHeaderSize(HEADER_SIZE);
        cfg.setMaxResponseHeaderSize(HEADER_SIZE);

        AbstractHTTP2ServerConnectionFactory h2;
        ServerConnector connector;
        if (tls) {
            SecureRequestCustomizer src = new SecureRequestCustomizer();
            src.setSniHostCheck(false);
            cfg.addCustomizer(src);
            h2 = new HTTP2ServerConnectionFactory(cfg);
            ALPNServerConnectionFactory alpn = new ALPNServerConnectionFactory("h2");
            alpn.setDefaultProtocol("h2");
            SslContextFactory.Server ssl = new SslContextFactory.Server();
            ssl.setKeyStore(keyStore((String) spec.get("cert"), (String) spec.get("key")));
            ssl.setKeyStorePassword("x");
            ssl.setCipherComparator(HTTP2Cipher.COMPARATOR);
            ssl.setSniRequired(false);
            SslConnectionFactory tlsFactory = new SslConnectionFactory(ssl, alpn.getProtocol());
            connector = new ServerConnector(server, tlsFactory, alpn, h2);
        } else {
            h2 = new HTTP2CServerConnectionFactory(cfg);
            connector = new ServerConnector(server, h2);
        }
        for (Map.Entry<String, Object> e : settings.entrySet()) {
            int v = (int) Math.min(((Number) e.getValue()).longValue(), Integer.MAX_VALUE);
            switch (e.getKey()) {
                case "SETTINGS_MAX_CONCURRENT_STREAMS" -> h2.setMaxConcurrentStreams(v);
                case "SETTINGS_INITIAL_WINDOW_SIZE" -> h2.setInitialStreamRecvWindow(v);
                case "SETTINGS_HEADER_TABLE_SIZE" -> h2.setMaxDecoderTableCapacity(v);
                case "SETTINGS_MAX_FRAME_SIZE" -> h2.setMaxFrameSize(v);
                case "SETTINGS_MAX_HEADER_LIST_SIZE" -> cfg.setRequestHeaderSize(v);
                default -> System.err.println("jetty server: ignoring unsupported setting " + e.getKey());
            }
        }
        connector.setHost(host);
        connector.setPort(port);
        server.addConnector(connector);
        server.setHandler(new Routes());
        server.start();
        System.err.println("jetty server: listening on " + addr + (tls ? " (TLS)" : " (h2c)"));
        server.join();
    }

    // keyStore returns a KeyStore holding the PEM certificate chain and
    // private key (PKCS#1 RSA or PKCS#8) from the named files.
    private static KeyStore keyStore(String certFile, String keyFile) throws Exception {
        List<Certificate> chain = new ArrayList<>();
        CertificateFactory cf = CertificateFactory.getInstance("X.509");
        try (var in = Files.newInputStream(Path.of(certFile))) {
            chain.addAll(cf.generateCertificates(in));
        }
        String pem = Files.readString(Path.of(keyFile));
        String b64 = pem.replaceAll("-----[A-Z ]+-----", "").replaceAll("\\s", "");
        byte[] der = Base64.getDecoder().decode(b64);
        // Don't trust the PEM label (the Go test key is PKCS#8 labeled
        // "RSA PRIVATE KEY"); try PKCS#8 first, then PKCS#1.
        PrivateKey key = null;
        for (String alg : new String[] {"RSA", "EC"}) {
            try {
                key = KeyFactory.getInstance(alg).generatePrivate(new PKCS8EncodedKeySpec(der));
                break;
            } catch (Exception e) {
                // Try the next format.
            }
        }
        if (key == null) {
            key = KeyFactory.getInstance("RSA").generatePrivate(new PKCS8EncodedKeySpec(pkcs1ToPkcs8(der)));
        }
        KeyStore store = KeyStore.getInstance("PKCS12");
        store.load(null, null);
        store.setKeyEntry("server", key, "x".toCharArray(), chain.toArray(new Certificate[0]));
        return store;
    }

    // pkcs1ToPkcs8 wraps a DER PKCS#1 RSAPrivateKey in a PKCS#8
    // PrivateKeyInfo, which is what the JDK's KeyFactory accepts.
    private static byte[] pkcs1ToPkcs8(byte[] pkcs1) {
        byte[] algId = {
            0x02, 0x01, 0x00, // version 0
            0x30, 0x0d, 0x06, 0x09, 0x2a, (byte) 0x86, 0x48, (byte) 0x86, (byte) 0xf7, 0x0d, 0x01, 0x01, 0x01, 0x05, 0x00,
        };
        byte[] octets = derTLV(0x04, pkcs1);
        byte[] body = new byte[algId.length + octets.length];
        System.arraycopy(algId, 0, body, 0, algId.length);
        System.arraycopy(octets, 0, body, algId.length, octets.length);
        return derTLV(0x30, body);
    }

    private static byte[] derTLV(int tag, byte[] v) {
        ByteArrayOutputStream out = new ByteArrayOutputStream();
        out.write(tag);
        int n = v.length;
        if (n < 0x80) {
            out.write(n);
        } else if (n < 0x100) {
            out.write(0x81);
            out.write(n);
        } else if (n < 0x10000) {
            out.write(0x82);
            out.write(n >> 8);
            out.write(n);
        } else {
            out.write(0x83);
            out.write(n >> 16);
            out.write(n >> 8);
            out.write(n);
        }
        out.write(v, 0, n);
        return out.toByteArray();
    }

    static byte[] pattern(int n) {
        byte[] b = new byte[n];
        for (int i = 0; i < n; i++) {
            b[i] = (byte) ALPHABET.charAt(i % ALPHABET.length());
        }
        return b;
    }

    static String headerPattern(int n) {
        StringBuilder sb = new StringBuilder(n);
        for (int i = 0; i < n; i++) {
            sb.append(HEADER_ALPHABET.charAt(i % HEADER_ALPHABET.length()));
        }
        return sb.toString();
    }

    // Routes implements the interop route set. Handlers run in pooled
    // threads and use Jetty's blocking helpers for simplicity.
    static final class Routes extends Handler.Abstract {
        @Override
        public boolean handle(Request req, Response resp, Callback cb) {
            try {
                route(req, resp, cb);
            } catch (Throwable t) {
                cb.failed(t);
            }
            return true;
        }

        private void route(Request req, Response resp, Callback cb) throws Exception {
            String path = req.getHttpURI().getPath();
            String[] parts = path.split("/", 3); // "", route, arg
            String route = parts.length > 1 ? parts[1] : "";
            String arg = parts.length > 2 ? parts[2] : null;
            HttpFields.Mutable h = resp.getHeaders();
            switch (route) {
                case "hello" -> {
                    h.put("content-type", "text/plain");
                    byte[] b = HELLO.getBytes(StandardCharsets.UTF_8);
                    h.put("content-length", b.length);
                    Content.Sink.write(resp, true, ByteBuffer.wrap(b));
                }
                case "bytes" -> {
                    int n = intArg(arg);
                    h.put("content-type", "application/octet-stream");
                    h.put("content-length", n);
                    if (!req.getMethod().equals("HEAD")) {
                        Content.Sink.write(resp, true, ByteBuffer.wrap(pattern(n)));
                    }
                }
                case "stream" -> {
                    int n = intArg(arg);
                    h.put("content-type", "application/octet-stream");
                    byte[] p = pattern(n);
                    for (int off = 0; off < n; off += 1000) {
                        Content.Sink.write(resp, false, ByteBuffer.wrap(p, off, Math.min(1000, n - off)));
                    }
                    Content.Sink.write(resp, true, ByteBuffer.allocate(0));
                }
                case "echo" -> {
                    h.put("content-type", "application/octet-stream");
                    long cl = req.getHeaders().getLongField("content-length");
                    if (cl >= 0) {
                        h.put("content-length", cl);
                    }
                    Content.Sink.write(resp, false, ByteBuffer.allocate(0)); // commit headers
                    for (;;) {
                        Content.Chunk c = readBlocking(req);
                        if (Content.Chunk.isFailure(c)) {
                            throw new IOException(c.getFailure());
                        }
                        ByteBuffer bb = c.getByteBuffer();
                        if (bb.hasRemaining()) {
                            ByteBuffer copy = ByteBuffer.allocate(bb.remaining());
                            copy.put(bb).flip();
                            Content.Sink.write(resp, false, copy);
                        }
                        boolean last = c.isLast();
                        c.release();
                        if (last) {
                            break;
                        }
                    }
                    Content.Sink.write(resp, true, ByteBuffer.allocate(0));
                }
                case "upload" -> {
                    MessageDigest md = MessageDigest.getInstance("SHA-256");
                    long n = 0;
                    Map<String, Object> trailer = null;
                    for (;;) {
                        Content.Chunk c = readBlocking(req);
                        if (Content.Chunk.isFailure(c)) {
                            throw new IOException(c.getFailure());
                        }
                        ByteBuffer bb = c.getByteBuffer();
                        n += bb.remaining();
                        md.update(bb);
                        if (c instanceof Trailers t) {
                            trailer = fieldMap(t.getTrailers());
                        }
                        boolean last = c.isLast();
                        c.release();
                        if (last) {
                            break;
                        }
                    }
                    Map<String, Object> m = new LinkedHashMap<>();
                    m.put("len", n);
                    m.put("sha256", HexFormat.of().formatHex(md.digest()));
                    if (trailer != null && !trailer.isEmpty()) {
                        m.put("trailer", trailer);
                    }
                    m.put("content_length", req.getHeaders().getLongField("content-length"));
                    writeJSON(resp, m);
                }
                case "trailers" -> {
                    h.put("content-type", "text/plain");
                    resp.setTrailersSupplier(() -> HttpFields.build()
                            .put("x-trailer-a", "1")
                            .put("x-trailer-b", "two"));
                    // Writing the body and completing separately keeps
                    // Jetty from adding a content-length, which would
                    // suppress the trailers.
                    Content.Sink.write(resp, false, ByteBuffer.wrap("trailers follow\n".getBytes(StandardCharsets.UTF_8)));
                    Content.Sink.write(resp, true, ByteBuffer.allocate(0));
                }
                case "status" -> {
                    resp.setStatus(intArg(arg));
                }
                case "bigheader" -> {
                    h.put("x-big", headerPattern(intArg(arg)));
                    okBody(resp);
                }
                case "manyheaders" -> {
                    int n = intArg(arg);
                    for (int i = 0; i < n; i++) {
                        h.put("x-h-" + i, "value-" + i);
                    }
                    okBody(resp);
                }
                case "early-hints" -> {
                    resp.writeInterim(103, HttpFields.build().put("link", "</style.css>; rel=preload; as=style")).get();
                    okBody(resp);
                }
                case "info" -> {
                    HttpURI uri = req.getHttpURI();
                    Map<String, Object> m = new LinkedHashMap<>();
                    m.put("method", req.getMethod());
                    m.put("path", uri.getPathQuery());
                    m.put("authority", uri.getAuthority());
                    m.put("proto", req.getConnectionMetaData().getProtocol());
                    m.put("header", fieldMap(req.getHeaders()));
                    writeJSON(resp, m);
                }
                case "delay" -> {
                    Thread.sleep(intArg(arg));
                    okBody(resp);
                }
                case "rst" -> {
                    h.put("content-type", "application/octet-stream");
                    Content.Sink.write(resp, false, ByteBuffer.wrap(pattern(1000)));
                    // Failing the callback of a committed response makes
                    // Jetty reset the stream, but with CANCEL, so reset it
                    // with INTERNAL_ERROR ourselves first.
                    if (req.getConnectionMetaData().getConnection() instanceof HTTP2Connection hc) {
                        int id = Integer.parseInt(req.getId());
                        Stream s = hc.getSession().getStream(id);
                        if (s != null) {
                            s.reset(new ResetFrame(id, ErrorCode.INTERNAL_ERROR.code)).get();
                        }
                    }
                    cb.failed(new IOException("h2interop: /rst route"));
                    return;
                }
                default -> {
                    resp.setStatus(404);
                    h.put("content-type", "text/plain");
                    Content.Sink.write(resp, true, ByteBuffer.wrap("not found\n".getBytes(StandardCharsets.UTF_8)));
                }
            }
            cb.succeeded();
        }

        private static int intArg(String s) {
            int n = Integer.parseInt(s);
            if (n < 0) {
                throw new IllegalArgumentException("negative");
            }
            return n;
        }

        private static void okBody(Response resp) throws IOException {
            Content.Sink.write(resp, true, ByteBuffer.wrap("ok\n".getBytes(StandardCharsets.UTF_8)));
        }

        private static void writeJSON(Response resp, Object v) throws IOException {
            byte[] b = Json.toJson(v).getBytes(StandardCharsets.UTF_8);
            resp.getHeaders().put("content-type", "application/json");
            resp.getHeaders().put("content-length", b.length);
            Content.Sink.write(resp, true, ByteBuffer.wrap(b));
        }

        private static Map<String, Object> fieldMap(HttpFields fields) {
            Map<String, Object> m = new LinkedHashMap<>();
            for (HttpField f : fields) {
                @SuppressWarnings("unchecked")
                List<Object> l = (List<Object>) m.computeIfAbsent(f.getLowerCaseName(), k -> new ArrayList<>());
                l.add(fromWire(f.getValue()));
            }
            return m;
        }

        // fromWire returns a field value's octets (which Jetty decodes
        // as ISO-8859-1) as UTF-8 if they're valid UTF-8.
        private static String fromWire(String v) {
            if (v == null) {
                return "";
            }
            for (int i = 0; i < v.length(); i++) {
                if (v.charAt(i) > 0xff) {
                    return v;
                }
            }
            String u = Client.utf8OrNull(v.getBytes(StandardCharsets.ISO_8859_1), Integer.MAX_VALUE);
            return u != null ? u : v;
        }

        // readBlocking returns the next request content chunk, waiting
        // for one if necessary.
        private static Content.Chunk readBlocking(Request req) throws InterruptedException {
            for (;;) {
                Content.Chunk c = req.read();
                if (c != null) {
                    return c;
                }
                CountDownLatch l = new CountDownLatch(1);
                req.demand(l::countDown);
                l.await();
            }
        }
    }
}
