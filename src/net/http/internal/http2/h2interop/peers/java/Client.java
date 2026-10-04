// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

import java.io.ByteArrayInputStream;
import java.io.ByteArrayOutputStream;
import java.io.PrintStream;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.ByteBuffer;
import java.nio.CharBuffer;
import java.nio.charset.CharacterCodingException;
import java.nio.charset.CodingErrorAction;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.cert.X509Certificate;
import java.time.Duration;
import java.util.ArrayList;
import java.util.HexFormat;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.CompletionException;
import java.util.concurrent.CompletionStage;
import java.util.concurrent.ExecutionException;
import java.util.concurrent.Flow;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.TimeoutException;
import javax.net.ssl.SSLContext;
import javax.net.ssl.TrustManager;
import javax.net.ssl.X509TrustManager;

// Client is the h2interop peer client using the JDK's java.net.http.HttpClient.
// It reads a ClientSpec JSON document on stdin and writes a ClientOutput
// JSON document to stdout. See ../README.md.
public final class Client {
    private static final String ALPHABET =
            "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ\n";

    // maxBody is the largest response body reported in full.
    private static final int MAX_BODY = 64 << 10;

    // overallTimeout bounds how long we wait for all requests, so that
    // we always produce output before the runner gives up on us.
    private static final long OVERALL_TIMEOUT_SEC = 50;

    public static void main(String[] args) throws Exception {
        PrintStream out = new PrintStream(System.out, false, StandardCharsets.UTF_8);
        Map<String, Object> output = new LinkedHashMap<>();
        List<Object> results = new ArrayList<>();
        output.put("results", results);
        try {
            String in = new String(System.in.readAllBytes(), StandardCharsets.UTF_8);
            @SuppressWarnings("unchecked")
            Map<String, Object> spec = (Map<String, Object>) Json.parse(in);
            run(spec, results);
        } catch (Throwable t) {
            t.printStackTrace();
            output.put("error", describe(t));
        }
        out.println(Json.toJson(output));
        out.flush();
        // HttpClient's selector thread is a daemon, but its executor
        // threads may linger; don't wait for them.
        System.exit(0);
    }

    @SuppressWarnings("unchecked")
    private static void run(Map<String, Object> spec, List<Object> results) throws Exception {
        String base = str(spec.get("url"));
        boolean concurrent = Boolean.TRUE.equals(spec.get("concurrent"));
        if (Boolean.TRUE.equals(spec.get("h2c"))) {
            // HttpClient can only reach h2c via an HTTP/1.1 Upgrade,
            // not with prior knowledge. Proceed anyway; the result
            // will show the protocol actually used.
            System.err.println("java client: h2c prior knowledge is not supported by HttpClient; will try Upgrade");
        }
        // HttpClient always derives :authority from the URI. Setting a
        // host header instead would make it send both, mismatched,
        // which RFC 9113 section 8.3.1 forbids, so the override is
        // ignored.
        if (!str(spec.get("authority")).isEmpty()) {
            System.err.println("java client: can't override :authority with HttpClient; ignoring authority "
                    + str(spec.get("authority")));
        }

        HttpClient client = HttpClient.newBuilder()
                .version(HttpClient.Version.HTTP_2)
                .sslContext(trustAllContext())
                .connectTimeout(Duration.ofSeconds(10))
                .followRedirects(HttpClient.Redirect.NEVER)
                .build();

        List<Object> reqs = (List<Object>) spec.get("requests");
        if (reqs == null) {
            reqs = List.of();
        }
        int n = reqs.size();
        Map<String, Object>[] res = new Map[n];
        long deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(OVERALL_TIMEOUT_SEC);

        if (concurrent) {
            // HttpClient doesn't coalesce concurrent attempts to
            // establish an HTTP/2 connection: requests started before
            // the first connection is up each get their own connection
            // (all but one of which are closed after use). To get the
            // requests multiplexed on one connection as the protocol
            // asks, the first request is completed alone before the
            // rest are started together.
            List<CompletableFuture<HttpResponse<BodyResult>>> futs = new ArrayList<>();
            for (int i = 0; i < n; i++) {
                Map<String, Object> rq = (Map<String, Object>) reqs.get(i);
                futs.add(send(client, base, rq));
                if (i == 0) {
                    res[0] = await(futs.get(0), deadline);
                }
            }
            for (int i = 1; i < n; i++) {
                res[i] = await(futs.get(i), deadline);
            }
        } else {
            for (int i = 0; i < n; i++) {
                Map<String, Object> rq = (Map<String, Object>) reqs.get(i);
                res[i] = await(send(client, base, rq), deadline);
            }
        }
        for (Map<String, Object> r : res) {
            results.add(r);
        }
    }

    // send starts the request rq.
    private static CompletableFuture<HttpResponse<BodyResult>> send(HttpClient client, String base, Map<String, Object> rq) {
        try {
            return client.sendAsync(buildRequest(base, rq), BodyResult.handler(num(rq.get("cancel_after"))));
        } catch (Throwable t) {
            return CompletableFuture.failedFuture(t);
        }
    }

    private static Map<String, Object> await(CompletableFuture<HttpResponse<BodyResult>> f, long deadline) {
        try {
            long wait = Math.max(deadline - System.nanoTime(), 0);
            return result(f.get(wait, TimeUnit.NANOSECONDS));
        } catch (TimeoutException e) {
            f.cancel(true);
            return errorResult("timeout waiting for response (" + OVERALL_TIMEOUT_SEC + "s overall)");
        } catch (Throwable t) {
            return errorResult(describe(t));
        }
    }

    @SuppressWarnings("unchecked")
    private static HttpRequest buildRequest(String base, Map<String, Object> rq) {
        String method = str(rq.get("method"));
        if (method.isEmpty()) {
            method = "GET";
        }
        int bodyLen = (int) num(rq.get("body_len"));
        boolean noContentLength = Boolean.TRUE.equals(rq.get("no_content_length"));

        HttpRequest.Builder b = HttpRequest.newBuilder(URI.create(base + str(rq.get("path"))))
                .version(HttpClient.Version.HTTP_2);
        if (Boolean.TRUE.equals(rq.get("expect_continue"))) {
            b.expectContinue(true);
        }
        List<Object> hdrs = (List<Object>) rq.get("header");
        if (hdrs != null) {
            for (Object o : hdrs) {
                List<Object> kv = (List<Object>) o;
                setHeader(b, str(kv.get(0)), str(kv.get(1)));
            }
        }
        List<Object> trailers = (List<Object>) rq.get("trailer");
        if (trailers != null && !trailers.isEmpty()) {
            System.err.println("java client: HttpClient can't send request trailers; ignoring them");
        }

        HttpRequest.BodyPublisher pub;
        if (bodyLen == 0) {
            pub = HttpRequest.BodyPublishers.noBody();
        } else if (noContentLength) {
            byte[] body = pattern(bodyLen);
            pub = HttpRequest.BodyPublishers.ofInputStream(() -> new ByteArrayInputStream(body));
        } else {
            pub = HttpRequest.BodyPublishers.ofByteArray(pattern(bodyLen));
        }
        switch (method) {
            case "GET":
                if (bodyLen == 0) {
                    b.GET();
                } else {
                    b.method(method, pub);
                }
                break;
            case "HEAD":
                if (bodyLen == 0) {
                    b.HEAD();
                } else {
                    b.method(method, pub);
                }
                break;
            case "POST":
                b.POST(pub);
                break;
            case "PUT":
                b.PUT(pub);
                break;
            default:
                b.method(method, pub);
        }
        return b.build();
    }

    // setHeader adds a request header field, converting HttpClient's
    // rejection (e.g. of restricted names) into a descriptive error.
    // HttpClient encodes header values as ISO-8859-1, so the value's
    // UTF-8 bytes are passed as ISO-8859-1 characters to put the
    // intended octets on the wire.
    private static void setHeader(HttpRequest.Builder b, String name, String value) {
        String wire = new String(value.getBytes(StandardCharsets.UTF_8), StandardCharsets.ISO_8859_1);
        try {
            b.header(name, wire);
        } catch (IllegalArgumentException e) {
            throw new IllegalArgumentException("HttpClient refused to send header " + name + ": " + e.getMessage(), e);
        }
    }

    private static Map<String, Object> result(HttpResponse<BodyResult> resp) {
        Map<String, Object> r = new LinkedHashMap<>();
        r.put("status", (long) resp.statusCode());
        r.put("proto", resp.version() == HttpClient.Version.HTTP_2 ? "h2" : "http/1.1");
        List<Object> hdr = new ArrayList<>();
        for (Map.Entry<String, List<String>> e : resp.headers().map().entrySet()) {
            String name = e.getKey().toLowerCase();
            if (name.startsWith(":")) {
                continue;
            }
            for (String v : e.getValue()) {
                hdr.add(List.of(name, fromWire(v)));
            }
        }
        r.put("header", hdr);
        BodyResult br = resp.body();
        r.put("body_len", br.len);
        r.put("body_sha256", br.sha256);
        if (br.body != null) {
            r.put("body", br.body);
        }
        if (br.canceled) {
            r.put("canceled", true);
        }
        return r;
    }

    private static Map<String, Object> errorResult(String err) {
        Map<String, Object> r = new LinkedHashMap<>();
        r.put("status", 0L);
        r.put("header", List.of());
        r.put("body_len", 0L);
        r.put("error", err);
        return r;
    }

    // fromWire reverses HttpClient's ISO-8859-1 decoding of a header
    // value, returning the value's octets as UTF-8 if they're valid
    // UTF-8, and the ISO-8859-1 decoding otherwise.
    private static String fromWire(String v) {
        for (int i = 0; i < v.length(); i++) {
            if (v.charAt(i) > 0xff) {
                return v;
            }
        }
        String u = utf8OrNull(v.getBytes(StandardCharsets.ISO_8859_1), Integer.MAX_VALUE);
        return u != null ? u : v;
    }

    // utf8OrNull returns b[0:n] decoded as UTF-8, or null if invalid.
    static String utf8OrNull(byte[] b, int n) {
        try {
            CharBuffer cb = StandardCharsets.UTF_8.newDecoder()
                    .onMalformedInput(CodingErrorAction.REPORT)
                    .onUnmappableCharacter(CodingErrorAction.REPORT)
                    .decode(ByteBuffer.wrap(b, 0, Math.min(n, b.length)));
            return cb.toString();
        } catch (CharacterCodingException e) {
            return null;
        }
    }

    // describe returns the class and message of the root cause of t.
    static String describe(Throwable t) {
        while ((t instanceof CompletionException || t instanceof ExecutionException) && t.getCause() != null) {
            t = t.getCause();
        }
        StringBuilder sb = new StringBuilder(t.getClass().getName());
        if (t.getMessage() != null) {
            sb.append(": ").append(t.getMessage());
        }
        for (Throwable c = t.getCause(); c != null && c != c.getCause(); c = c.getCause()) {
            sb.append(" (caused by ").append(c.getClass().getName());
            if (c.getMessage() != null) {
                sb.append(": ").append(c.getMessage());
            }
            sb.append(")");
        }
        return sb.toString();
    }

    static byte[] pattern(int n) {
        byte[] b = new byte[n];
        for (int i = 0; i < n; i++) {
            b[i] = (byte) ALPHABET.charAt(i % ALPHABET.length());
        }
        return b;
    }

    private static String str(Object o) {
        return o == null ? "" : o.toString();
    }

    private static long num(Object o) {
        return o instanceof Number num ? num.longValue() : 0;
    }

    private static SSLContext trustAllContext() throws Exception {
        TrustManager tm = new X509TrustManager() {
            @Override
            public void checkClientTrusted(X509Certificate[] chain, String authType) {}

            @Override
            public void checkServerTrusted(X509Certificate[] chain, String authType) {}

            @Override
            public X509Certificate[] getAcceptedIssuers() {
                return new X509Certificate[0];
            }
        };
        SSLContext ctx = SSLContext.getInstance("TLS");
        ctx.init(null, new TrustManager[] {tm}, null);
        return ctx;
    }

    // BodyResult is a response body summarized as its length, SHA-256,
    // and (if small and valid UTF-8) contents.
    static final class BodyResult {
        long len;
        String sha256;
        String body;
        boolean canceled;

        // handler returns a BodyHandler producing a BodyResult. If
        // cancelAfter is positive, the body subscription is canceled
        // (which resets the stream) once at least that many bytes have
        // been received.
        static HttpResponse.BodyHandler<BodyResult> handler(long cancelAfter) {
            return info -> new Sub(cancelAfter);
        }
    }

    // Sub consumes a response body, hashing it as it arrives.
    static final class Sub implements HttpResponse.BodySubscriber<BodyResult> {
        private final long cancelAfter;
        private final MessageDigest md;
        private final ByteArrayOutputStream head = new ByteArrayOutputStream();
        private final CompletableFuture<BodyResult> result = new CompletableFuture<>();
        private Flow.Subscription sub;
        private long len;

        Sub(long cancelAfter) {
            this.cancelAfter = cancelAfter;
            try {
                md = MessageDigest.getInstance("SHA-256");
            } catch (Exception e) {
                throw new RuntimeException(e);
            }
        }

        @Override
        public CompletionStage<BodyResult> getBody() {
            return result;
        }

        @Override
        public void onSubscribe(Flow.Subscription s) {
            sub = s;
            s.request(Long.MAX_VALUE);
        }

        @Override
        public void onNext(List<ByteBuffer> bufs) {
            if (result.isDone()) {
                return;
            }
            for (ByteBuffer bb : bufs) {
                int n = bb.remaining();
                len += n;
                if (head.size() <= MAX_BODY) {
                    byte[] b = new byte[n];
                    bb.duplicate().get(b);
                    head.write(b, 0, Math.min(n, MAX_BODY + 1 - head.size()));
                }
                md.update(bb);
            }
            if (cancelAfter > 0 && len >= cancelAfter) {
                // Complete the body before canceling, since canceling
                // fails the exchange.
                BodyResult r = summary();
                r.canceled = true;
                result.complete(r);
                sub.cancel();
            }
        }

        @Override
        public void onError(Throwable t) {
            result.completeExceptionally(t);
        }

        @Override
        public void onComplete() {
            result.complete(summary());
        }

        private BodyResult summary() {
            BodyResult r = new BodyResult();
            r.len = len;
            r.sha256 = HexFormat.of().formatHex(md.digest());
            if (len <= MAX_BODY) {
                r.body = utf8OrNull(head.toByteArray(), MAX_BODY);
            }
            return r;
        }
    }
}
