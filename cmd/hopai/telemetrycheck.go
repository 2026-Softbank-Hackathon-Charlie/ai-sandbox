package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/llm"
	"github.com/2026-Softbank-Hackathon-Charlie/ai-sandbox/mask"
)

func telemetryCheckMain(args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "usage: hopai telemetry-check")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	result, err := llm.TelemetryCheck(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, mask.String(err.Error()))
		return 1
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		return 1
	}
	return 0
}
