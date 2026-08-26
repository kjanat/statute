# One image per commit for the whole e2e lane: the Statute binary under
# test plus the origin and client actors, built from the exact tree the
# harness runs from. Build context is the repository root.
#
# The base images are digest-pinned (multi-arch manifests, amd64+arm64)
# so a run is reproducible and needs no registry reachability after the
# pre-pull step. The runtime image is the root-variant distroless: the
# per-node state volumes are created by the daemon as root-owned mounts.
FROM golang@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS builder
# golang:1.27-alpine
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -tags e2e -trimpath -ldflags="-s -w" -o /out/statute ./e2e/cmd/statute \
    && CGO_ENABLED=0 go build -tags e2e -trimpath -ldflags="-s -w" -o /out/origin ./e2e/cmd/origin \
    && CGO_ENABLED=0 go build -tags e2e -trimpath -ldflags="-s -w" -o /out/client ./e2e/cmd/client

FROM gcr.io/distroless/static-debian12@sha256:d75cdd72874d4790092fcb1b058493ecf6bb5bf2b2b897045b00ff01d91843f2
# gcr.io/distroless/static-debian12:latest
COPY --from=builder /out/statute /statute
COPY --from=builder /out/origin /origin
COPY --from=builder /out/client /client
ENTRYPOINT ["/statute"]
