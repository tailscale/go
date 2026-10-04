//! The h2interop server peer, using the h2 crate's server directly.
//! It's run as `server '<ServerSpec JSON>'` and serves the route set
//! from peers/README.md until killed.

use std::collections::BTreeMap;
use std::future::poll_fn;
use std::sync::Arc;
use std::time::Duration;

use bytes::Bytes;
use h2::server::SendResponse;
use h2::{Reason, RecvStream};
use h2interop_peer::*;
use http::{HeaderMap, HeaderValue, Method, Request, Response, StatusCode};
use rustls::pki_types::pem::PemObject;
use rustls::pki_types::{CertificateDer, PrivateKeyDer};
use serde::Serialize;
use sha2::{Digest, Sha256};
use tokio::io::{AsyncRead, AsyncWrite};
use tokio::net::TcpListener;

#[tokio::main]
async fn main() {
    if let Err(e) = run().await {
        eprintln!("server: fatal: {e}");
        std::process::exit(1);
    }
}

async fn run() -> Result<(), BoxError> {
    let arg = std::env::args()
        .nth(1)
        .ok_or("usage: server '<ServerSpec JSON>'")?;
    let spec: ServerSpec = serde_json::from_str(&arg)?;

    let mut builder = h2::server::Builder::new();
    for (name, &v) in &spec.settings {
        match name.as_str() {
            "SETTINGS_MAX_CONCURRENT_STREAMS" => {
                builder.max_concurrent_streams(v);
            }
            "SETTINGS_INITIAL_WINDOW_SIZE" => {
                builder.initial_window_size(v);
            }
            "SETTINGS_MAX_FRAME_SIZE" => {
                builder.max_frame_size(v);
            }
            "SETTINGS_HEADER_TABLE_SIZE" => {
                builder.header_table_size(v);
            }
            "SETTINGS_MAX_HEADER_LIST_SIZE" => {
                builder.max_header_list_size(v);
            }
            _ => eprintln!("server: ignoring unsupported setting {name}={v}"),
        }
    }
    let builder = Arc::new(builder);

    let acceptor = if spec.tls {
        Some(tokio_rustls::TlsAcceptor::from(Arc::new(tls_config(
            &spec.cert, &spec.key,
        )?)))
    } else {
        None
    };

    let ln = TcpListener::bind(&spec.addr).await?;
    eprintln!(
        "server: listening on {} (tls={}, settings={:?})",
        spec.addr, spec.tls, spec.settings
    );
    loop {
        let (tcp, peer) = match ln.accept().await {
            Ok(c) => c,
            Err(e) => {
                eprintln!("server: accept: {e}");
                tokio::time::sleep(Duration::from_millis(10)).await;
                continue;
            }
        };
        let _ = tcp.set_nodelay(true);
        let builder = builder.clone();
        let acceptor = acceptor.clone();
        tokio::spawn(async move {
            let res = match acceptor {
                Some(acceptor) => match acceptor.accept(tcp).await {
                    Ok(tls) => serve_conn(&builder, tls).await,
                    Err(e) => Err(format!("TLS handshake: {e}").into()),
                },
                None => serve_conn(&builder, tcp).await,
            };
            if let Err(e) = res {
                eprintln!("server: connection from {peer}: {e}");
            }
        });
    }
}

fn tls_config(cert: &str, key: &str) -> Result<rustls::ServerConfig, BoxError> {
    let certs = CertificateDer::pem_file_iter(cert)?.collect::<Result<Vec<_>, _>>()?;
    let key = PrivateKeyDer::from_pem_file(key)?;
    let build = |key: PrivateKeyDer<'static>| -> Result<rustls::ServerConfig, BoxError> {
        Ok(rustls::ServerConfig::builder_with_provider(crypto_provider())
            .with_safe_default_protocol_versions()?
            .with_no_client_auth()
            .with_single_cert(certs.clone(), key)?)
    };
    // The test key (net/http/internal/testcert) is PKCS #8 data in a
    // PEM block labeled "RSA PRIVATE KEY", which Go accepts but rustls
    // parses as PKCS #1. If that fails, retry it as PKCS #8.
    let mut config = match build(key.clone_key()) {
        Ok(c) => c,
        Err(e) => match &key {
            PrivateKeyDer::Pkcs1(k) => {
                eprintln!("server: key is not PKCS #1 ({e}); trying PKCS #8");
                build(PrivateKeyDer::Pkcs8(k.secret_pkcs1_der().to_vec().into()))?
            }
            _ => return Err(e),
        },
    };
    config.alpn_protocols = vec![b"h2".to_vec()];
    Ok(config)
}

async fn serve_conn<T>(builder: &h2::server::Builder, io: T) -> Result<(), BoxError>
where
    T: AsyncRead + AsyncWrite + Unpin + Send + 'static,
{
    let mut conn = builder.handshake::<T, Bytes>(io).await?;
    while let Some(r) = conn.accept().await {
        let (req, respond) = r?;
        tokio::spawn(async move {
            let what = format!("{} {}", req.method(), req.uri());
            if let Err(e) = handle(req, respond).await {
                eprintln!("server: {what}: {e}");
            }
        });
    }
    Ok(())
}

fn response(status: StatusCode) -> http::response::Builder {
    Response::builder().status(status)
}

/// Sends a complete response with the given body. If ctype is
/// non-empty it's sent as the content-type.
fn send_simple(
    respond: &mut SendResponse<Bytes>,
    status: StatusCode,
    ctype: &str,
    extra: &[(String, String)],
    body: Bytes,
) -> Result<Option<h2::SendStream<Bytes>>, BoxError> {
    let mut b = response(status).header("content-length", body.len());
    if !ctype.is_empty() {
        b = b.header("content-type", ctype);
    }
    for (k, v) in extra {
        b = b.header(k.as_str(), v.as_str());
    }
    let resp = b.body(())?;
    if body.is_empty() {
        respond.send_response(resp, true)?;
        return Ok(None);
    }
    Ok(Some(respond.send_response(resp, false)?))
}

async fn reply(
    respond: &mut SendResponse<Bytes>,
    status: StatusCode,
    ctype: &str,
    extra: &[(String, String)],
    body: Bytes,
) -> Result<(), BoxError> {
    if let Some(mut s) = send_simple(respond, status, ctype, extra, body.clone())? {
        send_body(&mut s, body, true).await?;
    }
    Ok(())
}

async fn bad_request(respond: &mut SendResponse<Bytes>, msg: &str) -> Result<(), BoxError> {
    reply(
        respond,
        StatusCode::BAD_REQUEST,
        "text/plain",
        &[],
        Bytes::from(format!("{msg}\n")),
    )
    .await
}

/// Parses the {n} in "/prefix/{n}".
fn path_int(rest: &str) -> Option<usize> {
    if rest.is_empty() || rest.contains('/') {
        return None;
    }
    rest.parse().ok()
}

async fn handle(req: Request<RecvStream>, mut respond: SendResponse<Bytes>) -> Result<(), BoxError> {
    let path = req.uri().path().to_string();
    let (route, rest) = match path[1.min(path.len())..].split_once('/') {
        Some((r, rest)) => (format!("/{r}"), rest.to_string()),
        None => (path.clone(), String::new()),
    };
    let has_param = path[1.min(path.len())..].contains('/');
    // The h2 crate leaves "expect: 100-continue" to the application.
    // Every handler is willing to read the body, so say so up front.
    let expects_continue = req
        .headers()
        .get(http::header::EXPECT)
        .is_some_and(|v| v.as_bytes().eq_ignore_ascii_case(b"100-continue"));
    if expects_continue {
        respond.send_informational(response(StatusCode::CONTINUE).body(())?)?;
    }
    match (route.as_str(), has_param) {
        ("/hello", false) => {
            reply(&mut respond, StatusCode::OK, "text/plain", &[], Bytes::from_static(b"Hello, HTTP/2 interop!\n")).await
        }
        ("/bytes", true) => {
            let Some(n) = path_int(&rest) else { return bad_request(&mut respond, "bad n").await };
            let resp = response(StatusCode::OK)
                .header("content-type", "application/octet-stream")
                .header("content-length", n)
                .body(())?;
            if n == 0 || req.method() == Method::HEAD {
                respond.send_response(resp, true)?;
                return Ok(());
            }
            let mut s = respond.send_response(resp, false)?;
            send_body(&mut s, pattern(n), true).await
        }
        ("/stream", true) => {
            let Some(n) = path_int(&rest) else { return bad_request(&mut respond, "bad n").await };
            let resp = response(StatusCode::OK)
                .header("content-type", "application/octet-stream")
                .body(())?;
            if n == 0 || req.method() == Method::HEAD {
                respond.send_response(resp, true)?;
                return Ok(());
            }
            let mut s = respond.send_response(resp, false)?;
            let p = pattern(n);
            let mut off = 0;
            while off < n {
                let c = (n - off).min(1000);
                // Wait for capacity for the whole chunk so that it goes
                // out as its own DATA frame.
                wait_capacity(&mut s, c, c).await?;
                s.send_data(p.slice(off..off + c), off + c == n)?;
                off += c;
            }
            Ok(())
        }
        ("/echo", false) => echo(req, respond).await,
        ("/upload", false) => upload(req, respond).await,
        ("/trailers", false) => {
            let resp = response(StatusCode::OK)
                .header("content-type", "text/plain")
                .header("trailer", "x-trailer-a, x-trailer-b")
                .body(())?;
            let mut s = respond.send_response(resp, false)?;
            send_body(&mut s, Bytes::from_static(b"trailers follow\n"), false).await?;
            let mut t = HeaderMap::new();
            t.insert("x-trailer-a", HeaderValue::from_static("1"));
            t.insert("x-trailer-b", HeaderValue::from_static("two"));
            s.send_trailers(t)?;
            Ok(())
        }
        ("/status", true) => {
            let code = path_int(&rest)
                .and_then(|c| u16::try_from(c).ok())
                .and_then(|c| StatusCode::from_u16(c).ok());
            let Some(code) = code else { return bad_request(&mut respond, "bad code").await };
            respond.send_response(response(code).body(())?, true)?;
            Ok(())
        }
        ("/bigheader", true) => {
            let Some(n) = path_int(&rest) else { return bad_request(&mut respond, "bad n").await };
            let extra = [("x-big".to_string(), header_pattern(n))];
            reply(&mut respond, StatusCode::OK, "text/plain", &extra, Bytes::from_static(b"ok\n")).await
        }
        ("/manyheaders", true) => {
            let Some(n) = path_int(&rest) else { return bad_request(&mut respond, "bad n").await };
            let extra: Vec<_> = (0..n).map(|i| (format!("x-h-{i}"), format!("value-{i}"))).collect();
            reply(&mut respond, StatusCode::OK, "text/plain", &extra, Bytes::from_static(b"ok\n")).await
        }
        ("/early-hints", false) => {
            let hints = response(StatusCode::EARLY_HINTS)
                .header("link", "</style.css>; rel=preload; as=style")
                .body(())?;
            respond.send_informational(hints)?;
            reply(&mut respond, StatusCode::OK, "text/plain", &[], Bytes::from_static(b"ok\n")).await
        }
        ("/info", false) => info(req, respond).await,
        ("/delay", true) => {
            let Some(ms) = path_int(&rest) else { return bad_request(&mut respond, "bad ms").await };
            tokio::select! {
                _ = tokio::time::sleep(Duration::from_millis(ms as u64)) => {}
                r = poll_fn(|cx| respond.poll_reset(cx)) => {
                    eprintln!("server: /delay: stream reset by client: {r:?}");
                    return Ok(());
                }
            }
            reply(&mut respond, StatusCode::OK, "text/plain", &[], Bytes::from_static(b"ok\n")).await
        }
        ("/rst", false) => {
            let resp = response(StatusCode::OK)
                .header("content-type", "application/octet-stream")
                .body(())?;
            let mut s = respond.send_response(resp, false)?;
            send_body(&mut s, pattern(1000), false).await?;
            // h2 discards a stream's queued frames when it's reset, so
            // give the connection a moment to write the DATA frame first.
            tokio::time::sleep(Duration::from_millis(50)).await;
            s.send_reset(Reason::INTERNAL_ERROR);
            Ok(())
        }
        _ => {
            reply(&mut respond, StatusCode::NOT_FOUND, "text/plain", &[], Bytes::from_static(b"404 page not found\n")).await
        }
    }
}

fn content_length(h: &HeaderMap) -> Option<u64> {
    h.get(http::header::CONTENT_LENGTH)?
        .to_str()
        .ok()?
        .trim()
        .parse()
        .ok()
}

/// Streams the request body back as it arrives.
async fn echo(req: Request<RecvStream>, mut respond: SendResponse<Bytes>) -> Result<(), BoxError> {
    let mut b = response(StatusCode::OK).header("content-type", "application/octet-stream");
    if let Some(cl) = content_length(req.headers()) {
        b = b.header("content-length", cl);
    }
    let mut s = respond.send_response(b.body(())?, false)?;
    let mut body = req.into_body();
    while let Some(chunk) = body.data().await {
        let chunk = chunk?;
        let n = chunk.len();
        send_body(&mut s, chunk, false).await?;
        let _ = body.flow_control().release_capacity(n);
    }
    s.send_data(Bytes::new(), true)?;
    Ok(())
}

#[derive(Serialize)]
struct UploadBody {
    len: u64,
    sha256: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    trailer: Option<BTreeMap<String, Vec<String>>>,
    content_length: i64,
}

async fn upload(req: Request<RecvStream>, mut respond: SendResponse<Bytes>) -> Result<(), BoxError> {
    let cl = content_length(req.headers()).map(|n| n as i64).unwrap_or(-1);
    let mut body = req.into_body();
    let mut h = Sha256::new();
    let mut len: u64 = 0;
    while let Some(chunk) = body.data().await {
        let chunk = chunk?;
        len += chunk.len() as u64;
        h.update(&chunk);
        let _ = body.flow_control().release_capacity(chunk.len());
    }
    let trailer = body.trailers().await?.filter(|t| !t.is_empty()).map(|t| header_map(&t));
    let ub = UploadBody {
        len,
        sha256: hex::encode(h.finalize()),
        trailer,
        content_length: cl,
    };
    let j = serde_json::to_vec(&ub)?;
    reply(&mut respond, StatusCode::OK, "application/json", &[], Bytes::from(j)).await
}

fn header_map(h: &HeaderMap) -> BTreeMap<String, Vec<String>> {
    let mut m: BTreeMap<String, Vec<String>> = BTreeMap::new();
    for (k, v) in h {
        m.entry(k.as_str().to_string())
            .or_default()
            .push(String::from_utf8_lossy(v.as_bytes()).into_owned());
    }
    m
}

#[derive(Serialize)]
struct InfoBody {
    method: String,
    path: String,
    authority: String,
    proto: String,
    header: BTreeMap<String, Vec<String>>,
}

async fn info(req: Request<RecvStream>, mut respond: SendResponse<Bytes>) -> Result<(), BoxError> {
    let authority = match req.uri().authority() {
        Some(a) => a.as_str().to_string(),
        None => req
            .headers()
            .get(http::header::HOST)
            .map(|h| String::from_utf8_lossy(h.as_bytes()).into_owned())
            .unwrap_or_default(),
    };
    let ib = InfoBody {
        method: req.method().as_str().to_string(),
        path: req
            .uri()
            .path_and_query()
            .map(|p| p.as_str().to_string())
            .unwrap_or_default(),
        authority,
        proto: "HTTP/2.0".into(),
        header: header_map(req.headers()),
    };
    // Drain any request body.
    let mut body = req.into_body();
    while let Some(chunk) = body.data().await {
        let chunk = chunk?;
        let _ = body.flow_control().release_capacity(chunk.len());
    }
    let j = serde_json::to_vec(&ib)?;
    reply(&mut respond, StatusCode::OK, "application/json", &[], Bytes::from(j)).await
}
