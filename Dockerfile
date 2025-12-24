FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git

WORKDIR /build

COPY go.mod go.sum ./

RUN go mod download

COPY *.go ./
COPY internal ./internal

RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -ldflags '-extldflags "-static"' -o pip-cache .

FROM alpine:latest

RUN apk --no-cache add ca-certificates tzdata wget

RUN addgroup -g 1000 app && \
    adduser -D -u 1000 -G app app

RUN mkdir -p /cache && chown app:app /cache

WORKDIR /app

COPY --from=builder /build/pip-cache .

USER app

COPY pipcache.yaml .

EXPOSE 8080

ENV PORT=8080 \
    CACHE_DIR=/cache \
    LOG_LEVEL=info

CMD ["./pip-cache"]
