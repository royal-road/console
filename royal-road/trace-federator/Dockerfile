# trace-federator: build context is this directory.
#
#   docker build -t trace-federator royal-road/trace-federator
#
# Standard library only, so no module download step.
FROM docker.io/library/golang:1.25 AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN go vet ./... && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/trace-federator .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/trace-federator /trace-federator
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/trace-federator"]

LABEL \
        org.opencontainers.image.source="https://github.com/royal-road/console" \
        org.opencontainers.image.description="Tempo-API federator over VictoriaTraces, SigNoz and Sentry for the Royal Road console" \
        org.opencontainers.image.licenses="Apache-2.0"
