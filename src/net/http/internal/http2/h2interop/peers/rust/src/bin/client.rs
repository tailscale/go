//! The h2interop client peer, using the h2 crate's client directly.
//! It reads a ClientSpec JSON document on stdin and writes a
//! ClientOutput JSON document to stdout.

use std::future::poll_fn;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use bytes::Bytes;
use h2::client::{ResponseFuture, SendRequest};
use h2::{Reason, SendStream};
use h2interop_peer::*;
use http::{HeaderMap, HeaderName, HeaderValue, Method, Request, Uri};
use sha2::{Digest, Sha256};
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite};
use tokio::net::TcpStream;
use tokio::sync::Notify;

const MAX_REPORTED_BODY: usize = 64 << 10;

#[tokio::main]
async fn main() {
    let mut input = Vec::new();
    let out = match tokio::io::stdin().read_to_end(&mut input).await {
        Err(e) => fatal(format!("reading stdin: {e}")),
        Ok(_) => match serde_json::from_slice::<ClientSpec>(&input) {
            Err(e) => fatal(format!("parsing spec: {e}")),
            Ok(spec) => match run(spec).await {
                Ok(out) => out,
                Err(e) => fatal(e.to_string()),
            },
        },
    };
    println!("{}", serde_json::to_string(&out).unwrap());
}

fn fatal(msg: String) -> ClientOutput {
    eprintln!("client: fatal: {msg}");
    ClientOutput {
        results: Vec::new(),
        error: msg,
    }
}

async fn run(spec: ClientSpec) -> Result<ClientOutput, BoxError> {
    let base: Uri = spec.url.parse()?;
    let scheme = base.scheme_str().unwrap_or("http").to_string();
    let hostport = base
        .authority()
        .ok_or("URL has no host")?
        .as_str()
        .to_string();
    let tcp = TcpStream::connect(&hostport).await?;
    tcp.set_nodelay(true)?;
    if scheme == "https" {
        let config = tls_config()?;
        let host = base.host().unwrap_or("").trim_matches(|c| c == '[' || c == ']');
        let server_name = rustls::pki_types::ServerName::try_from(host.to_string())?;
        let tls = tokio_rustls::TlsConnector::from(Arc::new(config))
            .connect(server_name, tcp)
            .await?;
        let alpn = tls.get_ref().1.alpn_protocol().map(|p| p.to_vec());
        if alpn.as_deref() != Some(b"h2") {
            return Err(format!(
                "server did not negotiate ALPN h2 (got {:?})",
                alpn.map(|p| String::from_utf8_lossy(&p).into_owned())
            )
            .into());
        }
        run_on(tls, &spec, &scheme, &hostport).await
    } else {
        run_on(tcp, &spec, &scheme, &hostport).await
    }
}

async fn run_on<T>(
    io: T,
    spec: &ClientSpec,
    scheme: &str,
    hostport: &str,
) -> Result<ClientOutput, BoxError>
where
    T: AsyncRead + AsyncWrite + Unpin + Send + 'static,
{
    let (sr, conn) = h2::client::handshake(io).await?;
    let conn_task = tokio::spawn(async move {
        if let Err(e) = conn.await {
            eprintln!("client: connection error: {e}");
        }
    });

    let authority = if spec.authority.is_empty() {
        hostport.to_string()
    } else {
        spec.authority.clone()
    };
    let base = format!("{scheme}://{authority}");

    let mut results = Vec::with_capacity(spec.requests.len());
    if spec.concurrent {
        let mut tasks = Vec::new();
        for r in &spec.requests {
            let sr = sr.clone();
            let r = r.clone();
            let base = base.clone();
            tasks.push(tokio::spawn(async move { do_request(sr, &base, &r).await }));
        }
        for t in tasks {
            results.push(match t.await {
                Ok(r) => r,
                Err(e) => ReqResult {
                    error: format!("request task failed: {e}"),
                    ..Default::default()
                },
            });
        }
    } else {
        for r in &spec.requests {
            results.push(do_request(sr.clone(), &base, r).await);
        }
    }
    drop(sr);
    // Give the connection a moment to flush any final frames (such as
    // RST_STREAM or WINDOW_UPDATE) before exiting.
    let _ = tokio::time::timeout(Duration::from_millis(200), conn_task).await;
    Ok(ClientOutput {
        results,
        error: String::new(),
    })
}

async fn do_request(sr: SendRequest<Bytes>, base: &str, r: &Req) -> ReqResult {
    let mut res = ReqResult::default();
    if let Err(e) = do_request_inner(sr, base, r, &mut res).await {
        res.error = e.to_string();
        eprintln!("client: {} {}: {}", r.method, r.path, res.error);
    }
    res
}

async fn do_request_inner(
    sr: SendRequest<Bytes>,
    base: &str,
    r: &Req,
    res: &mut ReqResult,
) -> Result<(), BoxError> {
    let uri: Uri = format!("{base}{}", r.path).parse()?;
    let mut req = Request::builder()
        .method(Method::from_bytes(r.method.as_bytes())?)
        .uri(uri)
        .body(())?;
    let h = req.headers_mut();
    for (k, v) in &r.header {
        h.append(
            HeaderName::from_bytes(k.as_bytes())?,
            HeaderValue::from_bytes(v.as_bytes())?,
        );
    }
    if r.body_len > 0 && !r.no_content_length {
        h.insert(http::header::CONTENT_LENGTH, HeaderValue::from(r.body_len));
    }
    if r.expect_continue {
        h.insert(http::header::EXPECT, HeaderValue::from_static("100-continue"));
    }
    let trailers = if r.trailer.is_empty() {
        None
    } else {
        let mut t = HeaderMap::new();
        for (k, v) in &r.trailer {
            t.append(
                HeaderName::from_bytes(k.as_bytes())?,
                HeaderValue::from_bytes(v.as_bytes())?,
            );
        }
        Some(t)
    };

    let end_stream = r.body_len == 0 && trailers.is_none();
    // Wait until the connection can accept a new stream (this
    // respects the server's SETTINGS_MAX_CONCURRENT_STREAMS).
    let mut sr = sr.ready().await?;
    let (resp_fut, send) = sr.send_request(req, end_stream)?;
    res.proto = "h2".into();
    let send = Arc::new(Mutex::new(send));
    let continue_ok = Arc::new(Notify::new());

    let sender = {
        let send = send.clone();
        let continue_ok = continue_ok.clone();
        let body_len = r.body_len;
        let expect_continue = r.expect_continue;
        async move {
            if end_stream {
                return Ok(());
            }
            if expect_continue && body_len > 0 {
                // Wait for the 100 (or a final response), but not forever
                // (RFC 9110, Section 10.1.1).
                let _ = tokio::time::timeout(Duration::from_secs(1), continue_ok.notified()).await;
            }
            send_request_body(&send, pattern(body_len), trailers).await
        }
    };
    let receiver = receive_response(resp_fut, &send, r, res, &continue_ok);
    let (send_res, recv_res) = tokio::join!(sender, receiver);
    recv_res?;
    if let Err(e) = send_res {
        if !res.canceled {
            return Err(format!("sending request body: {e}").into());
        }
    }
    Ok(())
}

/// Sends the request body and trailers. The SendStream is shared with
/// the receiver, which may reset the stream; the lock is never held
/// across an await.
async fn send_request_body(
    send: &Mutex<SendStream<Bytes>>,
    body: Bytes,
    trailers: Option<HeaderMap>,
) -> Result<(), BoxError> {
    let mut off = 0;
    while off < body.len() {
        let remaining = body.len() - off;
        send.lock().unwrap().reserve_capacity(remaining);
        let cap = loop {
            let c = send.lock().unwrap().capacity();
            if c > 0 {
                break c;
            }
            match poll_fn(|cx| send.lock().unwrap().poll_capacity(cx)).await {
                None => return Err("stream closed while waiting for send capacity".into()),
                Some(Err(e)) => return Err(Box::new(e)),
                Some(Ok(_)) => {}
            }
        };
        let n = cap.min(remaining);
        let last = off + n == body.len();
        send.lock()
            .unwrap()
            .send_data(body.slice(off..off + n), last && trailers.is_none())?;
        off += n;
    }
    if let Some(t) = trailers {
        send.lock().unwrap().send_trailers(t)?;
    }
    Ok(())
}

async fn receive_response(
    mut resp_fut: ResponseFuture,
    send: &Mutex<SendStream<Bytes>>,
    r: &Req,
    res: &mut ReqResult,
    continue_ok: &Notify,
) -> Result<(), BoxError> {
    // Collect any interim (1xx) responses before the final response.
    loop {
        match poll_fn(|cx| resp_fut.poll_informational(cx)).await {
            None => break,
            Some(Err(e)) => {
                continue_ok.notify_one();
                return Err(Box::new(e));
            }
            Some(Ok(inf)) => {
                if inf.status() == http::StatusCode::CONTINUE {
                    continue_ok.notify_one();
                }
                res.informational.push(Informational {
                    status: inf.status().as_u16(),
                    header: fields(inf.headers()),
                });
            }
        }
    }
    let resp = resp_fut.await;
    continue_ok.notify_one();
    let resp = resp?;
    res.status = resp.status().as_u16();
    res.header = fields(resp.headers());

    let mut body = resp.into_body();
    let mut hasher = Sha256::new();
    let mut kept: Vec<u8> = Vec::new();
    let mut total: u64 = 0;
    let mut err = None;
    while let Some(chunk) = body.data().await {
        let chunk = match chunk {
            Ok(c) => c,
            Err(e) => {
                err = Some(e);
                break;
            }
        };
        let n = chunk.len();
        total += n as u64;
        hasher.update(&chunk);
        if kept.len() <= MAX_REPORTED_BODY {
            kept.extend_from_slice(&chunk[..n.min(MAX_REPORTED_BODY + 1 - kept.len())]);
        }
        let _ = body.flow_control().release_capacity(n);
        if r.cancel_after > 0 && total >= r.cancel_after {
            send.lock().unwrap().send_reset(Reason::CANCEL);
            res.canceled = true;
            break;
        }
    }
    res.body_len = total;
    res.body_sha256 = hex::encode(hasher.finalize());
    if kept.len() <= MAX_REPORTED_BODY {
        if let Ok(s) = String::from_utf8(kept) {
            res.body = s;
        }
    }
    if let Some(e) = err {
        return Err(Box::new(e));
    }
    if res.canceled {
        return Ok(());
    }
    if let Some(t) = body.trailers().await? {
        res.trailer = fields(&t);
    }
    Ok(())
}

/// Returns a TLS client config offering ALPN h2 that doesn't verify
/// the server's certificate.
fn tls_config() -> Result<rustls::ClientConfig, BoxError> {
    let provider = crypto_provider();
    let mut config = rustls::ClientConfig::builder_with_provider(provider.clone())
        .with_safe_default_protocol_versions()?
        .dangerous()
        .with_custom_certificate_verifier(Arc::new(NoVerify(provider)))
        .with_no_client_auth();
    config.alpn_protocols = vec![b"h2".to_vec()];
    Ok(config)
}

#[derive(Debug)]
struct NoVerify(Arc<rustls::crypto::CryptoProvider>);

impl rustls::client::danger::ServerCertVerifier for NoVerify {
    fn verify_server_cert(
        &self,
        _end_entity: &rustls::pki_types::CertificateDer<'_>,
        _intermediates: &[rustls::pki_types::CertificateDer<'_>],
        _server_name: &rustls::pki_types::ServerName<'_>,
        _ocsp_response: &[u8],
        _now: rustls::pki_types::UnixTime,
    ) -> Result<rustls::client::danger::ServerCertVerified, rustls::Error> {
        Ok(rustls::client::danger::ServerCertVerified::assertion())
    }

    fn verify_tls12_signature(
        &self,
        message: &[u8],
        cert: &rustls::pki_types::CertificateDer<'_>,
        dss: &rustls::DigitallySignedStruct,
    ) -> Result<rustls::client::danger::HandshakeSignatureValid, rustls::Error> {
        rustls::crypto::verify_tls12_signature(
            message,
            cert,
            dss,
            &self.0.signature_verification_algorithms,
        )
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &rustls::pki_types::CertificateDer<'_>,
        dss: &rustls::DigitallySignedStruct,
    ) -> Result<rustls::client::danger::HandshakeSignatureValid, rustls::Error> {
        rustls::crypto::verify_tls13_signature(
            message,
            cert,
            dss,
            &self.0.signature_verification_algorithms,
        )
    }

    fn supported_verify_schemes(&self) -> Vec<rustls::SignatureScheme> {
        self.0.signature_verification_algorithms.supported_schemes()
    }
}
