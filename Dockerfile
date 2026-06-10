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
FROM alpine:latest
RUN apk add --no-cache tzdata
WORKDIR /root/

COPY --from=builder /app/kotman-engine .

EXPOSE 3000

CMD ["./kotman-engine"]