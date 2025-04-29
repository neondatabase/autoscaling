package main

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"time"
)

// Configuration constants
const (
	defaultDuration = 10 * time.Minute
	reportInterval  = 1 * time.Second
	bytesPerKB      = 1024.0
)

func main() {
	fmt.Println("Starting log rate benchmark")
	fmt.Println("Reading logs from stdin for", defaultDuration)
	fmt.Println("Press Ctrl+C to stop early")

	// Setup counters and wait group
	var messageCounter uint64
	var byteCounter uint64
	var wg sync.WaitGroup

	// Setup signal handling for graceful termination
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt)

	// Create a done channel to signal when benchmark should end
	done := make(chan struct{})

	// Start reading from stdin
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			select {
			case <-done:
				return
			default:
				line := scanner.Text()
				lineBytes := len(line) + 1 // +1 for the newline character
				atomic.AddUint64(&messageCounter, 1)
				atomic.AddUint64(&byteCounter, uint64(lineBytes))
			}
		}
		if err := scanner.Err(); err != nil {
			fmt.Fprintf(os.Stderr, "Error reading input: %v\n", err)
		}
	}()

	// Start a goroutine to periodically report stats
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(reportInterval)
		startTime := time.Now()
		lastReportTime := startTime
		lastMsgCount := uint64(0)
		lastByteCount := uint64(0)

		for {
			select {
			case <-ticker.C:
				now := time.Now()
				elapsed := now.Sub(startTime)
				currentMsgCount := atomic.LoadUint64(&messageCounter)
				currentByteCount := atomic.LoadUint64(&byteCounter)

				// Calculate overall rates
				overallMsgRate := float64(currentMsgCount) / elapsed.Seconds()
				overallKBRate := float64(currentByteCount) / bytesPerKB / elapsed.Seconds()

				// Calculate instantaneous rates
				intervalElapsed := now.Sub(lastReportTime)
				intervalMsgCount := currentMsgCount - lastMsgCount
				intervalByteCount := currentByteCount - lastByteCount
				instantMsgRate := float64(intervalMsgCount) / intervalElapsed.Seconds()
				instantKBRate := float64(intervalByteCount) / bytesPerKB / intervalElapsed.Seconds()

				fmt.Printf("[%s] Total: %d logs (%.2f KB) | Overall: %.2f logs/sec (%.2f KB/sec) | Current: %.2f logs/sec (%.2f KB/sec)\n",
					elapsed.Round(time.Second),
					currentMsgCount,
					float64(currentByteCount)/bytesPerKB,
					overallMsgRate,
					overallKBRate,
					instantMsgRate,
					instantKBRate)

				lastReportTime = now
				lastMsgCount = currentMsgCount
				lastByteCount = currentByteCount

			case <-done:
				ticker.Stop()
				return
			}
		}
	}()

	// Start a timer for the benchmark duration
	timer := time.NewTimer(defaultDuration)

	// Wait for timer to expire or for user interrupt
	select {
	case <-timer.C:
		fmt.Println("\nBenchmark duration complete")
	case <-signalChan:
		fmt.Println("\nBenchmark interrupted by user")
	}

	// Signal reader to stop and wait for goroutines to finish
	close(done)
	wg.Wait()

	// Final report
	finalMsgCount := atomic.LoadUint64(&messageCounter)
	finalByteCount := atomic.LoadUint64(&byteCounter)
	totalDuration := time.Since(time.Now().Add(-defaultDuration))
	finalMsgRate := float64(finalMsgCount) / totalDuration.Seconds()
	finalKBRate := float64(finalByteCount) / bytesPerKB / totalDuration.Seconds()

	fmt.Println("\n--- Benchmark Results ---")
	fmt.Printf("Duration: %s\n", totalDuration.Round(time.Millisecond))
	fmt.Printf("Total logs processed: %d (%.2f KB)\n", finalMsgCount, float64(finalByteCount)/bytesPerKB)
	fmt.Printf("Average rate: %.2f logs/second (%.2f KB/second)\n", finalMsgRate, finalKBRate)
}
