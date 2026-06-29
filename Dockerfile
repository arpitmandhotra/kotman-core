# ==========================================
# STAGE 1: THE BUILDER
# ==========================================
FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o kotman-engine ./cmd/main.go

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

WORKDIR /home/kotman

COPY --from=builder --chown=kotman:kotman /app/kotman-engine .

USER kotman

EXPOSE 3000

CMD ["./kotman-engine"]