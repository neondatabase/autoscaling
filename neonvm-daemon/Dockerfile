FROM golang:1.23-alpine AS builder

# Build the Go binary
COPY . .

# Build
RUN CGO_ENABLED=0 go build -o /neonvmd neonvm-daemon/cmd/*.go

FROM scratch
COPY --from=builder /neonvmd /neonvmd
