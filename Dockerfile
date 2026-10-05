# syntax=docker/dockerfile:1

# Stage 1: CSS with the Tailwind standalone CLI, no Node. The output does not
# depend on the target platform, so it runs on the build platform.
FROM --platform=$BUILDPLATFORM debian:trixie-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a AS css
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY scripts/tailwind.sh scripts/
COPY internal/web/css internal/web/css
COPY internal/web/templates internal/web/templates
RUN ./scripts/tailwind.sh

# Stage 2: static Go binary, cross-compiled for the target platform.
FROM --platform=$BUILDPLATFORM golang:1.27.1-trixie@sha256:3b77fc618ec235a1ab412de7737f120dd507c57e8d87de4cbb7994fb94275ed5 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=css /src/internal/web/static/app.css internal/web/static/app.css
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/sos-vpdive ./cmd/sos-vpdive \
 && mkdir /out/data

# Stage 3: distroless, non-root, no shell. Only /data is written.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
COPY --from=build /out/sos-vpdive /sos-vpdive
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/sos-vpdive", "healthcheck"]
ENTRYPOINT ["/sos-vpdive"]
CMD ["serve"]
