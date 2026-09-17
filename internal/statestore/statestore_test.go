package statestore

import (
	"path/filepath"
	"testing"
	"time"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.sqlite3")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestOpenEmptyPathReturnsNil(t *testing.T) {
	store, err := Open("")
	if err != nil || store != nil {
		t.Fatalf("Open(\"\") = %v, %v; want nil, nil", store, err)
	}
}

func TestUpsertTargetAndSnapshot(t *testing.T) {
	store := openTemp(t)
	if err := store.UpsertTarget(Target{
		Host: "https://gitea.example", Repository: "acme/rocket",
		Dir: "/data/repos/rocket", BaseBranch: "main", Managed: true, Ready: true, Provider: "minimax",
	}); err != nil {
		t.Fatalf("UpsertTarget: %v", err)
	}
	// 幂等 upsert：改 ready 状态
	if err := store.UpsertTarget(Target{
		Host: "https://gitea.example", Repository: "acme/rocket", Ready: false, SkipReason: "克隆失败",
	}); err != nil {
		t.Fatalf("UpsertTarget 2: %v", err)
	}
	snapshot, err := store.Snapshot(10)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snapshot.Targets) != 1 || snapshot.Targets[0].Ready || snapshot.Targets[0].SkipReason != "克隆失败" {
		t.Fatalf("targets = %+v", snapshot.Targets)
	}
}

func TestReplaceQueue(t *testing.T) {
	store := openTemp(t)
	now := time.Now()
	items := []Item{{Kind: "issue", Number: 1, Title: "a"}, {Kind: "issue", Number: 2, Title: "b"}}
	if err := store.ReplaceQueue("https://gitea.example", "acme/rocket", now, items); err != nil {
		t.Fatalf("ReplaceQueue: %v", err)
	}
	// 第二轮只剩一个：整体替换而非追加
	if err := store.ReplaceQueue("https://gitea.example", "acme/rocket", now, items[:1]); err != nil {
		t.Fatalf("ReplaceQueue 2: %v", err)
	}
	snapshot, _ := store.Snapshot(10)
	if len(snapshot.Queue) != 1 || snapshot.Queue[0].Number != 1 {
		t.Fatalf("queue = %+v", snapshot.Queue)
	}
}

func TestSessionMutexAndLifecycle(t *testing.T) {
	store := openTemp(t)
	now := time.Now()
	item := Item{Kind: "issue", Number: 7}

	// 首次启动成功，第二次被拒（同一待办同时只一个 claude）
	ok, err := store.StartSession("h", "r", item, now, "sid-1", time.Hour)
	if err != nil || !ok {
		t.Fatalf("first StartSession = %v, %v", ok, err)
	}
	ok, err = store.StartSession("h", "r", item, now.Add(time.Second), "sid-2", time.Hour)
	if err != nil || ok {
		t.Fatalf("second StartSession 应被互斥拒绝 = %v, %v", ok, err)
	}
	// 其他待办不受影响
	ok, _ = store.StartSession("h", "r", Item{Kind: "issue", Number: 8}, now, "sid-3", time.Hour)
	if !ok {
		t.Fatal("不同待办不应互斥")
	}

	if err := store.FinishSession("h", "r", item, now, Result{
		Subtype: "success", Turns: 3, CostUSD: 0.05, DurationMS: 1500, SessionID: "sid-1", FinishedAt: now,
	}); err != nil {
		t.Fatalf("FinishSession: %v", err)
	}
	// 结束后互斥解除
	ok, err = store.StartSession("h", "r", item, now.Add(2*time.Second), "sid-4", time.Hour)
	if err != nil || !ok {
		t.Fatalf("结束后应可再启动 = %v, %v", ok, err)
	}
}

func TestStaleRunningTakeover(t *testing.T) {
	store := openTemp(t)
	now := time.Now()
	item := Item{Kind: "pull", Number: 9}
	// 模拟崩溃残留：2 小时前的 running
	if ok, err := store.StartSession("h", "r", item, now.Add(-2*time.Hour), "dead", time.Hour); err != nil || !ok {
		t.Fatalf("StartSession: %v, %v", ok, err)
	}
	// staleAfter=1h：残留被接管
	ok, err := store.StartSession("h", "r", item, now, "new", time.Hour)
	if err != nil || !ok {
		t.Fatalf("残留应被接管 = %v, %v", ok, err)
	}
	snapshot, _ := store.Snapshot(10)
	if len(snapshot.Sessions) != 2 {
		t.Fatalf("sessions = %+v", snapshot.Sessions)
	}
	// 残留那条应是 error 态
	for _, session := range snapshot.Sessions {
		if session.SessionID == "dead" && session.State != "error" {
			t.Fatalf("残留会话应为 error: %+v", session)
		}
	}
}
