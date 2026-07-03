# ==========================================
# STAGE 1: BUILDER COMMON
# ==========================================
FROM golang:1.26-alpine AS builder-common

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# ==========================================
# STAGE 2: API COMPILING
# ==========================================
FROM builder-common AS api-builder

RUN CGO_ENABLED=0 GOOS=linux go build -buildvcs=false -o /app/api-server ./cmd/main.go

# ==========================================
# STAGE 3: WORKER COMPILING
# ==========================================
FROM builder-common AS worker-builder

RUN CGO_ENABLED=0 GOOS=linux go build -buildvcs=false -o /app/bg-worker ./cmd/worker/

# ==========================================
# STAGE 4: API RUNTIME MINIMAL
# ==========================================
FROM alpine:latest AS api

RUN apk add --no-cache tzdata

# Create a non-root user with a fixed UID/GID for security hardening
RUN addgroup -g 1001 -S kotman && \
    adduser -u 1001 -S kotman -G kotman

# Use /app as the working directory (owned by root, write-protected for kotman user)
WORKDIR /app

# Copy the statically linked binary
COPY --from=api-builder /app/api-server /app/api-server

# Drop root privileges
USER kotman

EXPOSE 8080

CMD ["/app/api-server"]

# ==========================================
# STAGE 5: WORKER RUNTIME MINIMAL
# ==========================================
FROM alpine:latest AS worker

RUN apk add --no-cache tzdata

# Create a non-root user with a fixed UID/GID for security hardening
RUN addgroup -g 1001 -S kotman && \
    adduser -u 1001 -S kotman -G kotman

# Use /app as the working directory (owned by root, write-protected for kotman user)
WORKDIR /app

# Copy the statically linked binary
COPY --from=worker-builder /app/bg-worker /app/bg-worker

# Drop root privileges
USER kotman

CMD ["/app/bg-worker"]