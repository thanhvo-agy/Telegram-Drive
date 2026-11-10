# Multi-stage ultra-lightweight Dockerfile for TeleClone (tgdrive)
# Final image size: ~15MB (compared to >500MB with Postgres/Teldrive)
FROM golang:1.24-alpine AS builder

WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download || true

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /tgdrive ./cmd/tgdrive

FROM alpine:latest
RUN apk --no-cache add ca-certificates tzdata
WORKDIR /app
COPY --from=builder /tgdrive /usr/local/bin/tgdrive

EXPOSE 8088
ENTRYPOINT ["/usr/local/bin/tgdrive"]
CMD ["serve", "--port", "8088"]
