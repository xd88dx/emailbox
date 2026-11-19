package service

import (
	"context"
	"testing"
	"time"
)

// 限流按服务商隔离:同一家占满槽位后,别家必须还能立刻拿到许可。
// 这条断言守住「一家把 8 个 worker 全占住」这个退化场景。
func TestProviderThrottleIsolatesProviders(t *testing.T) {
	throttle := NewMailJobThrottle(1, 1000)
	ctx := context.Background()

	releaseQQ, err := throttle.acquire(ctx, "qq")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseQQ()

	blocked, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := throttle.acquire(blocked, "qq"); err == nil {
		t.Fatal("同一服务商在并发已满时仍然拿到了许可")
	}

	release163, err := throttle.acquire(ctx, "163")
	if err != nil {
		t.Fatalf("不同服务商被互相阻塞: %v", err)
	}
	release163()
}
