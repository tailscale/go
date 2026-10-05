# Rust peer for h2interop: a client and server written directly
# against the h2 crate (hyperium/h2) with tokio and rustls (ring).

FROM rust:1-alpine AS build
RUN apk add --no-cache musl-dev
WORKDIR /src

# Build the dependencies first, so edits to src/ don't rebuild them.
COPY Cargo.toml Cargo.lock ./
RUN mkdir -p src/bin && \
    echo 'fn main() {}' > src/bin/client.rs && \
    echo 'fn main() {}' > src/bin/server.rs && \
    touch src/lib.rs && \
    cargo build --release --locked && \
    rm -rf src

COPY src ./src
RUN touch src/lib.rs src/bin/*.rs && cargo build --release --locked

FROM alpine:3.22
COPY --from=build /src/target/release/client /usr/local/bin/client
COPY --from=build /src/target/release/server /usr/local/bin/server
ENTRYPOINT []
CMD ["server"]
