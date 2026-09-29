# One image per commit for the whole e2e lane: the Statute binary under
# test plus the origin and client actors, built from the exact tree the
# harness runs from. Build context is the repository root.
#
# The base images are digest-pinned (multi-arch manifests, amd64+arm64)
# so a run is reproducible and needs no registry reachability after the
# pre-pull step. The tag in front of the digest is load-bearing, not
# decoration: without it a digest is an untagged reference that resolves
# against :latest, and a "digest refresh" would swap the image outright.
# The runtime image is the root-variant distroless: the
# per-node state volumes are created by the daemon as root-owned mounts.
FROM golang:1.27-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -tags e2e -trimpath -ldflags="-s -w" -o /out/statute ./e2e/cmd/statute \
    && CGO_ENABLED=0 go build -tags e2e -trimpath -ldflags="-s -w" -o /out/origin ./e2e/cmd/origin \
    && CGO_ENABLED=0 go build -tags e2e -trimpath -ldflags="-s -w" -o /out/client ./e2e/cmd/client \
    && CGO_ENABLED=0 go build -tags e2e -trimpath -ldflags="-s -w" -o /out/dockerproxy ./e2e/cmd/dockerproxy

FROM gcr.io/distroless/static-debian12:latest@sha256:d75cdd72874d4790092fcb1b058493ecf6bb5bf2b2b897045b00ff01d91843f2
COPY --from=builder /out/statute /statute
COPY --from=builder /out/origin /origin
COPY --from=builder /out/client /client
COPY --from=builder /out/dockerproxy /dockerproxy
ENTRYPOINT ["/statute"]
