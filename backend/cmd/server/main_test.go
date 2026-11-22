package main

import (
	"context"
	"os"
	"regexp"
	"sync"
	"testing"
	"time"
)

// The shutdown fits compose.yaml's stop_grace_period: past it Docker kills the process and the writer's tail and
// the last counters are lost.
func TestShutdownBudget(t *testing.T) {
	b, err := os.ReadFile("../../../compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s+stop_grace_period:\s*(\S+)`).FindSubmatch(b)
	if m == nil {
		t.Fatal("compose.yaml sets no stop_grace_period: Docker's 10 s default is shorter than the shutdown")
	}
	grace, err := time.ParseDuration(string(m[1]))
	if err != nil {
		t.Fatal(err)
	}
	if shutdownBudget >= grace || grace-shutdownBudget < 2*time.Second {
		t.Errorf("shutdown budget %v, stop_grace_period %v: no margin", shutdownBudget, grace)
	}
}

func TestStopWithin(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { <-ctx.Done() })
	if !stopWithin(cancel, &wg, time.Second) {
		t.Error("work that stops on cancel reported stuck")
	}
	release := make(chan struct{})
	wg.Go(func() { <-release })
	if stopWithin(func() {}, &wg, 20*time.Millisecond) {
		t.Error("stuck work reported stopped")
	}
	close(release)
	wg.Wait()
}
