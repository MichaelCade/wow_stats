# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /app

# Allow the Go toolchain to satisfy the toolchain directive in go.mod
ENV GOTOOLCHAIN=auto

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -o wow_stats .

# Runtime stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates

WORKDIR /root/

# Copy the binary from builder
COPY --from=builder /app/wow_stats .

# Copy the images directory
COPY images/ ./images/

# Expose port
EXPOSE 8080

# Run the application
CMD ["./wow_stats"]
