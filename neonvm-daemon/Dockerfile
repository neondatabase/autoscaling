ARG GO_BASE_IMG=autoscaling-go-base:dev
FROM $GO_BASE_IMG AS builder

# Build the Go binary
COPY . .

# Build
RUN CGO_ENABLED=0 go build -o /neonvmd neonvm-daemon/cmd/*.go

FROM scratch
COPY --from=builder /neonvmd /neonvmd
