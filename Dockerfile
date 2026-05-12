FROM golang:1.23-alpine AS builder
WORKDIR /build
COPY go.mod ./
COPY . .
RUN go mod tidy && CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w" \
    -trimpath \
    -o /bestproxy ./cmd/bestproxy

FROM scratch
COPY --from=builder /bestproxy /bestproxy
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
EXPOSE 8888
ENTRYPOINT ["/bestproxy"]
CMD ["--config", "/config.yaml"]
