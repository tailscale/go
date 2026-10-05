//! Shared code for the h2interop Rust peer (h2 crate) client and server.
//! See ../README.md (peers/README.md) for the protocol.

use std::future::poll_fn;
use std::sync::Arc;

use bytes::Bytes;
use h2::SendStream;
use serde::{Deserialize, Serialize};

pub type BoxError = Box<dyn std::error::Error + Send + Sync>;

const ALPHABET: &[u8] = b"0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ\n";

/// Returns the n-byte body pattern.
pub fn pattern(n: usize) -> Bytes {
    let mut v = Vec::with_capacity(n);
    for i in 0..n {
        v.push(ALPHABET[i % ALPHABET.len()]);
    }
    Bytes::from(v)
}

/// Returns the n-byte header-safe pattern.
pub fn header_pattern(n: usize) -> String {
    const A: &[u8] = b"abcdefghijklmnopqrstuvwxyz0123456789";
    (0..n).map(|i| A[i % A.len()] as char).collect()
}

#[derive(Deserialize, Debug, Default)]
#[serde(default)]
pub struct ClientSpec {
    pub url: String,
    pub h2c: bool,
    pub authority: String,
    pub concurrent: bool,
    pub requests: Vec<Req>,
}

#[derive(Deserialize, Debug, Default, Clone)]
#[serde(default)]
pub struct Req {
    pub method: String,
    pub path: String,
    pub header: Vec<(String, String)>,
    pub body_len: usize,
    pub no_content_length: bool,
    pub trailer: Vec<(String, String)>,
    pub expect_continue: bool,
    pub cancel_after: u64,
}

#[derive(Serialize, Debug, Default)]
pub struct ClientOutput {
    pub results: Vec<ReqResult>,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub error: String,
}

#[derive(Serialize, Debug, Default)]
pub struct ReqResult {
    pub status: u16,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub proto: String,
    pub header: Vec<(String, String)>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub trailer: Vec<(String, String)>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub informational: Vec<Informational>,
    pub body_len: u64,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub body_sha256: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub body: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub error: String,
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    pub canceled: bool,
}

#[derive(Serialize, Debug, Default)]
pub struct Informational {
    pub status: u16,
    pub header: Vec<(String, String)>,
}

#[derive(Deserialize, Debug, Default)]
#[serde(default)]
pub struct ServerSpec {
    pub addr: String,
    pub tls: bool,
    pub cert: String,
    pub key: String,
    pub settings: std::collections::HashMap<String, u32>,
}

/// Converts a header map to (name, value) pairs in iteration order.
/// HeaderMap iterates names in order of first appearance, with all
/// values of a name grouped together in received order.
pub fn fields(h: &http::HeaderMap) -> Vec<(String, String)> {
    h.iter()
        .map(|(k, v)| {
            (
                k.as_str().to_string(),
                String::from_utf8_lossy(v.as_bytes()).into_owned(),
            )
        })
        .collect()
}

/// Waits until the stream has at least min bytes of send capacity
/// (or less if want is smaller), having reserved want bytes.
/// It returns the available capacity.
pub async fn wait_capacity(
    s: &mut SendStream<Bytes>,
    want: usize,
    min: usize,
) -> Result<usize, BoxError> {
    let min = min.min(want).max(1);
    s.reserve_capacity(want);
    loop {
        let c = s.capacity();
        if c >= min {
            return Ok(c);
        }
        match poll_fn(|cx| s.poll_capacity(cx)).await {
            None => return Err("stream closed while waiting for send capacity".into()),
            Some(Err(e)) => return Err(Box::new(e)),
            Some(Ok(_)) => {}
        }
    }
}

/// Sends data on s, respecting flow control by only sending as much
/// as the peer has granted capacity for. If end_stream is set, the
/// last DATA frame carries END_STREAM.
pub async fn send_body(
    s: &mut SendStream<Bytes>,
    data: Bytes,
    end_stream: bool,
) -> Result<(), BoxError> {
    if data.is_empty() {
        if end_stream {
            s.send_data(Bytes::new(), true)?;
        }
        return Ok(());
    }
    let mut off = 0;
    while off < data.len() {
        let remaining = data.len() - off;
        let cap = wait_capacity(s, remaining, 1).await?;
        let n = cap.min(remaining);
        let last = off + n == data.len();
        s.send_data(data.slice(off..off + n), last && end_stream)?;
        off += n;
    }
    s.reserve_capacity(0);
    Ok(())
}

/// Returns a rustls crypto provider (ring).
pub fn crypto_provider() -> Arc<rustls::crypto::CryptoProvider> {
    Arc::new(rustls::crypto::ring::default_provider())
}
