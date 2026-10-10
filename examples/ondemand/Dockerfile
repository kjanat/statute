FROM golang:1.27-alpine@sha256:738d1cf061836894ff6bb8c33881080ac66de8cf0586615012a0c8f592649cfa AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/proxy ./examples/ondemand \
    && CGO_ENABLED=0 go build -trimpath -o /out/origin ./examples/ondemand/origin

FROM gcr.io/distroless/static-debian12:latest@sha256:d75cdd72874d4790092fcb1b058493ecf6bb5bf2b2b897045b00ff01d91843f2
COPY --from=builder /out/proxy /proxy
COPY --from=builder /out/origin /origin
ENTRYPOINT ["/proxy"]
