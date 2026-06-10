FROM --platform=$BUILDPLATFORM golang:1.25-alpine3.21 AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /app
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /app/fsb -ldflags="-w -s" ./cmd/fsb

FROM alpine:3.21
RUN apk add --no-cache ffmpeg tzdata ca-certificates ttf-dejavu fontconfig
WORKDIR /app
COPY --from=builder /app/fsb /app/fsb
COPY --from=builder /app/web /app/web
RUN mkdir -p /app/logs
EXPOSE ${PORT}
ENTRYPOINT ["/app/fsb", "run"]
