# alert-renderer: build context is this directory.
#
#   docker build -t alert-renderer royal-road/alert-renderer
#
# The image holds one static musl binary and nothing else: no shell, no
# package manager, no libc, no CA bundle (rustls carries the webpki roots)
# and no fonts (one is compiled in).
FROM docker.io/library/rust:1.100-alpine3.24 AS build
WORKDIR /src
# Dependencies first, so source changes do not rebuild them.
COPY Cargo.toml Cargo.lock ./
RUN mkdir src && echo 'fn main() {}' > src/main.rs && \
    cargo build --release --locked && rm -rf src
COPY assets ./assets
COPY src ./src
RUN touch src/main.rs && cargo build --release --locked && \
    cp target/release/alert-renderer /alert-renderer

FROM scratch
COPY --from=build /alert-renderer /alert-renderer
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/alert-renderer"]

LABEL \
        org.opencontainers.image.source="https://github.com/royal-road/console" \
        org.opencontainers.image.description="Alertmanager webhook receiver that posts readable Discord messages with a chart of the rule" \
        org.opencontainers.image.licenses="Apache-2.0"
