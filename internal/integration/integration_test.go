package integration

import (
	"context"
	"testing"
	"time"
)

// SleepCtx 的正常路径：context 未取消时必须真的等满时长再返回（微信 ret=-14
// 的重试节流靠它，提前返回会变成热循环打爆平台限频）。
func TestSleepCtxWaitsOutFullTimer(t *testing.T) {
	start := time.Now()
	SleepCtx(t.Context(), 50*time.Millisecond)
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("应等满 50ms，实际只等了 %v", elapsed)
	}
}

// SleepCtx 的取消路径：context 取消后必须立刻返回而不是继续睡——daemon 退出时
// 通道协程要能马上收尾，否则最长会拖住一小时的退避睡眠（微信 ret=-14 分支）。
func TestSleepCtxReturnsImmediatelyOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	start := time.Now()
	SleepCtx(ctx, time.Hour)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("已取消的 context 应立即返回，实际等了 %v", elapsed)
	}
}
