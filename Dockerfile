FROM golang:1.24-alpine AS builder

WORKDIR /src

ARG TARGETOS=linux
ARG TARGETARCH=amd64

COPY go.mod go.sum* ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/tg-video-archiver ./cmd/tg-video-archiver

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S app && \
    adduser -S -G app app

WORKDIR /app
COPY --from=builder /out/tg-video-archiver /app/tg-video-archiver

RUN mkdir -p /data/telegram-videos /data/telegram-session && chown -R app:app /data/telegram-videos /data/telegram-session /app
USER app

ENV STORAGE_ROOT=/data/telegram-videos
ENV MT_SESSION_FILE=/data/telegram-session/session.json

ENTRYPOINT ["/app/tg-video-archiver"]
