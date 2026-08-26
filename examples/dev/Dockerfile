# Multi-stage build for the dev statute proxy.
FROM golang:1.27-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /statute ./examples/dev

FROM gcr.io/distroless/static-debian12
COPY --from=builder /statute /statute
EXPOSE 8080 9090
ENTRYPOINT ["/statute"]
