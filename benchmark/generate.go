package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// GenerateLogs generates logs at the specified rate (in KB/s) until stopped
func GenerateLogs(kbPerSecond int) error {
	const bytesPerKB = 1024

	// Set up signal handling for graceful termination
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Calculate bytes per second
	bytesPerSecond := int64(kbPerSecond) * bytesPerKB

	// Create a buffer for our log messages
	// Using a size that's reasonable but larger for high throughput
	baseMessageSize := 4096
	message := make([]byte, baseMessageSize)

	// Fill the buffer with some repeatable but not totally uniform content
	for i := 0; i < baseMessageSize; i++ {
		message[i] = byte('A' + (i % 26))
	}

	// Calculate how many messages we need to send per second
	messagesPerSecond := bytesPerSecond / int64(baseMessageSize)
	if messagesPerSecond < 1 {
		messagesPerSecond = 1
	}

	// For very high rates, we'll need to send multiple messages per tick
	var ticksPerSecond, messagesPerTick int64

	// Cap the maximum tick rate to avoid timer resolution issues
	const maxTicksPerSecond = 1000 // Max 1000 ticks per second (1ms resolution)

	if messagesPerSecond <= maxTicksPerSecond {
		ticksPerSecond = messagesPerSecond
		messagesPerTick = 1
	} else {
		ticksPerSecond = maxTicksPerSecond
		messagesPerTick = (messagesPerSecond + ticksPerSecond - 1) / ticksPerSecond
	}

	// Ensure we have a valid tick interval (must be > 0)
	tickInterval := time.Second / time.Duration(ticksPerSecond)
	if tickInterval < time.Microsecond {
		tickInterval = time.Microsecond // Minimum tick interval
	}

	fmt.Fprintf(os.Stderr, "Target rate: %d KB/s (%d bytes/s)\n", kbPerSecond, bytesPerSecond)
	fmt.Fprintf(os.Stderr, "Log message size: %d bytes\n", baseMessageSize)
	fmt.Fprintf(os.Stderr, "Sending %d messages per tick, %d ticks per second (interval: %v)\n",
		messagesPerTick, ticksPerSecond, tickInterval)

	// Set up a ticker to regulate our sending
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	// Counter for statistics
	var messageCount int64
	startTime := time.Now()

	// Stats reporting ticker (once per second)
	statsTicker := time.NewTicker(time.Second)
	defer statsTicker.Stop()

	// Main log generation loop
	for {
		select {
		case <-ticker.C:
			// Send multiple messages per tick for high rates
			for i := int64(0); i < messagesPerTick; i++ {
				fmt.Printf("[%s] LOG-%d: %s\n",
					time.Now().Format(time.RFC3339Nano),
					messageCount,
					string(message))

				messageCount++
			}

		case <-statsTicker.C:
			// Print periodic stats to stderr
			elapsed := time.Since(startTime).Seconds()
			actualBytes := float64(messageCount * int64(baseMessageSize))
			actualKB := actualBytes / bytesPerKB
			actualRate := actualKB / elapsed

			fmt.Fprintf(os.Stderr, "Stats: %d msgs, %.2f KB, %.2f KB/s\n",
				messageCount, actualKB, actualRate)

		case <-sigChan:
			// Final statistics on exit
			elapsed := time.Since(startTime).Seconds()
			actualBytes := float64(messageCount * int64(baseMessageSize))
			actualKB := actualBytes / bytesPerKB
			actualRate := actualKB / elapsed

			fmt.Fprintf(os.Stderr, "\nFinal stats: %d msgs, %.2f KB, %.2f KB/s in %.2f seconds\n",
				messageCount, actualKB, actualRate, elapsed)
			return nil
		}
	}
}

func main() {
	var kbPerSecond int
	var duration time.Duration
	flag.IntVar(&kbPerSecond, "rate", 1000, "Rate in KB/s")
	flag.DurationVar(&duration, "duration", 10*time.Second, "Duration of the benchmark")
	flag.Parse()

	done := make(chan bool)
	go func() {
		GenerateLogs(kbPerSecond)
		done <- true
	}()

	time.Sleep(duration)
	close(done)
	<-done
}
