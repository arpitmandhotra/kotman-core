# ==========================================
# STAGE 1: THE BUILDER
# ==========================================
FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Compile all four binaries
RUN CGO_ENABLED=0 GOOS=linux go build -o bin/server ./cmd/main.go
RUN CGO_ENABLED=0 GOOS=linux go build -o bin/worker ./cmd/worker/recovery_worker.go
RUN CGO_ENABLED=0 GOOS=linux go build -o bin/migrate ./cmd/migrate/main.go
RUN CGO_ENABLED=0 GOOS=linux go build -o bin/purge ./cmd/purge/main.go

# ==========================================
# STAGE 2: THE PRODUCTION RUNNER
# ==========================================
FROM alpine:3.21

RUN apk add --no-cache tzdata

# Create a non-root user with a fixed UID/GID.
# The binary runs as this user — if compromised, the attacker
# cannot escalate to root or modify system files.
RUN addgroup -g 1001 -S kotman && \
    adduser -u 1001 -S kotman -G kotman

WORKDIR /app

COPY --from=builder --chown=kotman:kotman /app/bin /app/bin

USER kotman

EXPOSE 3000

CMD ["/app/bin/server"]