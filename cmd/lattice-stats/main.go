package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

type Telemetry struct {
	RequestID     string  `json:"request_id"`
	TotalTime     float64 `json:"total_time_s"`
	ExecutionTime float64 `json:"execution_time_s"`
	Target        string  `json:"target"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: lattice-stats <telemetry-file>")
		os.Exit(1)
	}

	filePath := os.Args[1]
	file, err := os.Open(filePath)
	if err != nil {
		fmt.Printf("Could not open telemetry log %s: %v\n", filePath, err)
		os.Exit(1)
	}
	defer file.Close()

	var totalCount int
	var targetCounts = make(map[string]int)
	var totalLatency float64

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var t Telemetry
		if err := json.Unmarshal(scanner.Bytes(), &t); err != nil {
			continue
		}
		totalCount++
		targetCounts[t.Target]++
		totalLatency += t.TotalTime
	}

	fmt.Printf("=== Lattice Telemetry Summary (%s) ===\n", filePath)
	fmt.Printf("Total Requests: %d\n", totalCount)
	if totalCount > 0 {
		fmt.Printf("Average Latency: %.2fs\n", totalLatency/float64(totalCount))
	}
	fmt.Println("\nRouting Distribution:")
	for target, count := range targetCounts {
		fmt.Printf("- %s: %d (%.1f%%)\n", target, count, float64(count)/float64(totalCount)*100)
	}
}
