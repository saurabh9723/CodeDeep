// Command mockllm is a deterministic OpenAI-compatible server used by the
// end-to-end tests and the evaluation harness.
package main

import (
	"flag"
	"log"
	"os"
	"time"

	"codedeep/internal/mockllm"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18080", "listen address")
	scenario := flag.String("scenario", "basic", "basic, multi, subagent, read, write, text, todo, memory, flaky, xss, mcp, mcp-ro")
	scriptPath := flag.String("script", "", "JSON step script (replaces the scenario for assistant turns)")
	delay := flag.Duration("delay", 25*time.Millisecond, "delay between streamed chunks")
	flag.Parse()

	var steps []mockllm.Step
	if *scriptPath != "" {
		data, err := os.ReadFile(*scriptPath)
		if err != nil {
			log.Fatalf("read script: %v", err)
		}
		script, err := mockllm.ParseScript(data)
		if err != nil {
			log.Fatalf("parse script: %v", err)
		}
		steps = script.Steps
	}
	if *scenario == "xss" {
		if err := mockllm.SeedXSSFixture("."); err != nil {
			log.Printf("xss fixture: %v", err)
		}
	}
	if err := mockllm.ListenAndServe(*addr, mockllm.Options{
		Scenario: *scenario,
		Steps:    steps,
		Delay:    *delay,
		Logger:   log.Default(),
	}); err != nil {
		log.Fatal(err)
	}
}
