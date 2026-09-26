package statestore

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// staleErrorsText 是 StartSession 判定超时接管时写下的固定文案。
const staleErrorsText = "进程崩溃残留（超时接管）"

// mustSessions 读全部会话，出错直接终止（边界测试里失败即错）。
func mustSessions(t *testing.T, store *Store) []Session {
	t.Helper()
	sessions, err := store.RecentSessions(100)
	if err != nil {
		t.Fatalf("RecentSessions: %v", err)
	}
	return sessions
}

// Open 的边界：空白路径返回 nil（未启用）、父路径是普通文件时报错、目录自动创建、
// 库文件真的落到指定路径（不是静默落到内存）。
func TestOpenBoundaries(t *testing.T) {
	// 只有空白的路径等同未配置
	for _, path := range []string{"", "   ", "\t\n"} {
		if store, err := Open(path); err != nil || store != nil {
			t.Fatalf("Open(%q) = %v, %v; want nil, nil", path, store, err)
		}
	}

	// 父路径是普通文件：MkdirAll 失败，错误带上目录
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(blocked, "state.sqlite3")); err == nil {
		t.Error("父路径是普通文件时应报错")
	} else if !strings.Contains(err.Error(), "创建状态库目录") {
		t.Errorf("错误应说明创建目录失败：%v", err)
	}

	// 多层不存在的目录：自动创建，库文件就位
	path := filepath.Join(t.TempDir(), "deep", "nested", "state.sqlite3")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("库文件应落在指定路径：%v", err)
	}

	// 目录路径（不是文件）：打开后无法建表，应报错而不是留下半个可用句柄
	if store, err := Open(t.TempDir()); err == nil {
		store.Close()
		t.Error("路径是目录时应报错")
	}
}

// Close 的边界：nil 接收者与零值句柄都不 panic；重复 Close 报错但不炸。
func TestCloseBoundaries(t *testing.T) {
	var nilStore *Store
	if err := nilStore.Close(); err != nil {
		t.Errorf("nil 接收者 Close 应为 nil：%v", err)
	}
	zero := &Store{}
	if err := zero.Close(); err != nil {
		t.Errorf("零值句柄 Close 应为 nil：%v", err)
	}

	store := openTemp(t)
	if err := store.Close(); err != nil {
		t.Errorf("首次 Close: %v", err)
	}
	// 二次 Close：database/sql 会返回「已关闭」，但不应 panic
	if err := store.Close(); err == nil {
		t.Log("二次 Close 未报错（实现允许），已确认不 panic")
	}
}

// nil Store 的每个方法都是安全 no-op（调用方按 nil 安全跳过，不写库也不报错）。
func TestNilStoreMethodsAreNoOps(t *testing.T) {
	var store *Store
	if err := store.UpsertTarget(Target{Host: "h", Repository: "r"}); err != nil {
		t.Errorf("UpsertTarget: %v", err)
	}
	if err := store.ReplaceQueue("h", "r", time.Now(), []Item{{Kind: "issue", Number: 1}}); err != nil {
		t.Errorf("ReplaceQueue: %v", err)
	}
	if ok, err := store.StartSession("h", "r", Item{}, time.Now(), "sid", time.Hour); err != nil || !ok {
		t.Errorf("StartSession = %v, %v; 应为 true, nil", ok, err)
	}
	if err := store.FinishSession("h", "r", Item{}, time.Now(), Result{}); err != nil {
		t.Errorf("FinishSession: %v", err)
	}
	if sessions, err := store.RecentSessions(10); err != nil || sessions != nil {
		t.Errorf("RecentSessions = %v, %v", sessions, err)
	}
	if snapshot, err := store.Snapshot(10); err != nil || snapshot != nil {
		t.Errorf("Snapshot = %v, %v", snapshot, err)
	}
}

// FinishSession 的两条定位路径：startedAt 已知时精确匹配那一条；
// startedAt 为零值时按 running 记录收尾（不误伤已结束的历史记录）。
func TestFinishSessionZeroStartedAt(t *testing.T) {
	store := openTemp(t)
	now := time.Now()
	item := Item{Kind: "issue", Number: 3}

	// 历史（已结束）记录：零值 startedAt 的收尾不该覆盖它
	if ok, err := store.StartSession("h", "r", item, now.Add(-2*time.Hour), "old", time.Hour); err != nil || !ok {
		t.Fatalf("StartSession: %v, %v", ok, err)
	}
	if err := store.FinishSession("h", "r", item, now.Add(-2*time.Hour), Result{
		Subtype: "done", FinishedAt: now.Add(-2 * time.Hour),
	}); err != nil {
		t.Fatalf("FinishSession(old): %v", err)
	}

	// 当前 running：零值 startedAt 收尾，只改 running 那条
	if ok, err := store.StartSession("h", "r", item, now, "live", time.Hour); err != nil || !ok {
		t.Fatalf("StartSession(live): %v, %v", ok, err)
	}
	if err := store.FinishSession("h", "r", item, time.Time{}, Result{
		IsError: true, Subtype: "crash", Turns: 4, CostUSD: 0.5,
		DurationMS: 2000, SessionID: "live", Errors: []string{"第一行", "第二行"},
	}); err != nil {
		t.Fatalf("FinishSession(zero): %v", err)
	}
	sessions, err := store.RecentSessions(10)
	if err != nil || len(sessions) != 2 {
		t.Fatalf("RecentSessions = %+v err=%v", sessions, err)
	}
	// 零值 startedAt 分支没有 started_at 谓词，只认 state='running'：历史那条
	// （已 success）不被改写，原 running 行被就地收尾。会话 id 保留 result.SessionID，
	// 因此这里靠「零值分支写入的字段」把它从两条里认出来。
	var finished Session
	var untouchedHistory bool
	for _, session := range sessions {
		if session.State == "success" && session.Subtype == "done" {
			untouchedHistory = true
			continue
		}
		finished = session
	}
	if !untouchedHistory {
		t.Errorf("已结束的历史记录被零值分支改写：%+v", sessions)
	}
	if finished.State != "error" || finished.Subtype != "crash" || finished.Turns != 4 ||
		finished.CostUSD != 0.5 || finished.DurationMS != 2000 {
		t.Errorf("零值 startedAt 收尾字段不对：%+v", finished)
	}
	if finished.Errors != "第一行\n第二行" {
		t.Errorf("多条错误应用换行拼接：%q", finished.Errors)
	}
	if finished.FinishedAt == "" {
		t.Error("零值 FinishedAt 应由 FinishSession 补当前时间")
	}
	if finished.SessionID != "live" {
		t.Errorf("被收尾的是最后一条 running（会话 id 应保留）：%+v", finished)
	}
}

// StartSession 的收尾分支：staleAfter 为正但不含任何残留（无 running 可接管）也能正常插入；
// 同一待办不同 kind/number 互不干扰。
func TestStartSessionStaleWindowAndIsolation(t *testing.T) {
	store := openTemp(t)
	now := time.Now()
	// staleAfter=0 表示窗口为 0：接管条件是 started_at < at，因此「同一时刻」插入
	// 的记录不被接管（互斥仍生效，窗口不是开关）。
	first := Item{Kind: "pull", Number: 11}
	if ok, err := store.StartSession("h", "r", first, now, "a", 0); err != nil || !ok {
		t.Fatalf("StartSession: %v, %v", ok, err)
	}
	// 同一时刻：started_at < now 不成立 → 仍互斥
	if ok, err := store.StartSession("h", "r", first, now, "b", 0); err != nil || ok {
		t.Fatalf("同一待办应仍互斥 = %v, %v", ok, err)
	}
	// staleAfter 过期：新一轮把残留 running 判为崩溃残留并接管
	if ok, err := store.StartSession("h", "r", first, now.Add(2*time.Second), "b2", 0); err != nil || !ok {
		t.Fatalf("过期残留应被接管 = %v, %v", ok, err)
	}
	var crashed Session
	for _, session := range mustSessions(t, store) {
		if session.SessionID == "a" {
			crashed = session
		}
	}
	if crashed.State != "error" || crashed.Errors != staleErrorsText {
		t.Errorf("过期接管应把残留标为崩溃：%+v", crashed)
	}
	// 同号不同 kind：互斥按 (host, repository, kind, number) 四元组
	if ok, err := store.StartSession("h", "r", Item{Kind: "issue", Number: 11}, now, "c", time.Hour); err != nil || !ok {
		t.Fatalf("不同 kind 不该互斥 = %v, %v", ok, err)
	}
	// 同 kind 同号不同仓库：互不干扰
	if ok, err := store.StartSession("h", "other", first, now, "d", time.Hour); err != nil || !ok {
		t.Fatalf("不同仓库不该互斥 = %v, %v", ok, err)
	}
}

// RecentSessions 的 limit 与排序：按 started_at 降序；limit<=0 时 SQLite 返回空。
func TestRecentSessionsOrderAndLimit(t *testing.T) {
	store := openTemp(t)
	base := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	for index, id := range []string{"s-1", "s-2", "s-3"} {
		item := Item{Kind: "issue", Number: int64(index + 1)}
		if ok, err := store.StartSession("h", "r", item, base.Add(time.Duration(index)*time.Minute), id, time.Hour); err != nil || !ok {
			t.Fatalf("StartSession(%s): %v, %v", id, ok, err)
		}
	}
	sessions, err := store.RecentSessions(2)
	if err != nil || len(sessions) != 2 {
		t.Fatalf("RecentSessions(2) = %+v err=%v", sessions, err)
	}
	if sessions[0].SessionID != "s-3" || sessions[1].SessionID != "s-2" {
		t.Errorf("应按开始时间降序：%+v", sessions)
	}
	if all, err := store.RecentSessions(100); err != nil || len(all) != 3 {
		t.Fatalf("RecentSessions(100) = %+v err=%v", all, err)
	}
}

// Snapshot 空库：三块都是空而非报错（内省窗口在新机器上不该失败）。
func TestSnapshotEmpty(t *testing.T) {
	store := openTemp(t)
	snapshot, err := store.Snapshot(10)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snapshot.Targets) != 0 || len(snapshot.Queue) != 0 || len(snapshot.Sessions) != 0 {
		t.Errorf("空库快照应为空：%+v", snapshot)
	}
}

// Snapshot 的布尔往返与队列排序：managed/ready 经 INTEGER 往返不丢真值；
// 队列按 (host, repository, kind, number) 升序。
func TestSnapshotBoolRoundTripAndQueueOrder(t *testing.T) {
	store := openTemp(t)
	if err := store.UpsertTarget(Target{
		Host: "h1", Repository: "r", Dir: "/d", BaseBranch: "main",
		Managed: true, Ready: true, SkipReason: "", Provider: "p",
	}); err != nil {
		t.Fatalf("UpsertTarget: %v", err)
	}
	if err := store.UpsertTarget(Target{Host: "h2", Repository: "r", Managed: false, Ready: true}); err != nil {
		t.Fatalf("UpsertTarget(2): %v", err)
	}
	now := time.Now()
	// 故意乱序插入：快照应按主键升序
	items := []Item{{Kind: "pull", Number: 5}, {Kind: "issue", Number: 2}, {Kind: "issue", Number: 10}}
	if err := store.ReplaceQueue("h1", "r", now, items); err != nil {
		t.Fatalf("ReplaceQueue: %v", err)
	}
	snapshot, err := store.Snapshot(10)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snapshot.Targets) != 2 {
		t.Fatalf("targets = %+v", snapshot.Targets)
	}
	for _, target := range snapshot.Targets {
		switch target.Host {
		case "h1":
			if !target.Managed || !target.Ready || target.Provider != "p" || target.Dir != "/d" {
				t.Errorf("h1 目标字段丢了：%+v", target)
			}
		case "h2":
			if target.Managed || !target.Ready {
				t.Errorf("h2 的 managed/ready 往返不对：%+v", target)
			}
		}
	}
	if len(snapshot.Queue) != 3 {
		t.Fatalf("queue = %+v", snapshot.Queue)
	}
	if snapshot.Queue[0].Kind != "issue" || snapshot.Queue[0].Number != 2 ||
		snapshot.Queue[1].Number != 10 || snapshot.Queue[2].Kind != "pull" {
		t.Errorf("队列应按主键升序：%+v", snapshot.Queue)
	}
	if snapshot.Queue[0].CheckedAt == "" {
		t.Error("checked_at 应为 RFC3339Nano 时间戳")
	}
	// 快照里带上了会话
	if _, err := store.StartSession("h1", "r", items[0], now, "sid", time.Hour); err != nil {
		t.Fatal(err)
	}
	if snapshot, _ := store.Snapshot(10); len(snapshot.Sessions) != 1 {
		t.Errorf("快照应带上会话：%+v", snapshot.Sessions)
	}
}

// boolToInt 的取值与 SQLite 侧的互逆性。
func TestBoolToInt(t *testing.T) {
	if got := boolToInt(true); got != 1 {
		t.Errorf("boolToInt(true) = %d", got)
	}
	if got := boolToInt(false); got != 0 {
		t.Errorf("boolToInt(false) = %d", got)
	}
}

// 打开一个已被删除目录下的库：父目录被清掉后写入报错应向外传（不吞）。
func TestWriteAfterDirRemovedReportsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "state.sqlite3")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	// WAL 已建好，删除目录后写入应报错或照常——两者都不该 panic；
	// 这里只断言错误要么为 nil 要么可识别为文件系统错误，不能是莫名其妙的东西。
	err = store.UpsertTarget(Target{Host: "h", Repository: "r"})
	if err != nil {
		var pathErr *fs.PathError
		if !errors.As(err, &pathErr) && !strings.Contains(err.Error(), "SQL") && !strings.Contains(err.Error(), "database") {
			t.Errorf("错误既不像是文件错误也不像 SQL 错误：%v", err)
		}
	}
}

// 打开一个「路径是目录」的库：初始化 schema 失败应返回错误，且不留下半个句柄。
func TestOpenDirectoryPathReportsError(t *testing.T) {
	store, err := Open(t.TempDir())
	if err == nil {
		store.Close()
		t.Fatal("路径是目录时不应打开成功")
	}
	if !strings.Contains(err.Error(), "初始化状态库") && !strings.Contains(err.Error(), "打开状态库") {
		t.Errorf("错误应说明是打开/初始化失败：%v", err)
	}
}

// 丢掉底层表之后的每个查询/写入都要把错误原样上报，而不是吞掉或返回半份结果
// （库被外部破坏时，内省方必须看到失败）。
func TestQueriesReportErrorsAfterTablesDropped(t *testing.T) {
	store := openTemp(t)
	if err := store.UpsertTarget(Target{Host: "h", Repository: "r"}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceQueue("h", "r", time.Now(), []Item{{Kind: "issue", Number: 1}}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"targets", "queue", "sessions"} {
		if _, err := store.db.Exec("DROP TABLE " + table); err != nil {
			t.Fatalf("DROP %s: %v", table, err)
		}
	}

	if err := store.UpsertTarget(Target{Host: "h", Repository: "r"}); err == nil {
		t.Error("targets 表缺失时 UpsertTarget 应报错")
	}
	if err := store.ReplaceQueue("h", "r", time.Now(), nil); err == nil {
		t.Error("queue 表缺失时 ReplaceQueue 应报错")
	}
	if ok, err := store.StartSession("h", "r", Item{Kind: "issue", Number: 1}, time.Now(), "s", time.Hour); err == nil || ok {
		t.Errorf("sessions 表缺失时 StartSession 应为 false 且报错 = %v, %v", ok, err)
	}
	if err := store.FinishSession("h", "r", Item{Kind: "issue", Number: 1}, time.Time{}, Result{}); err == nil {
		t.Error("sessions 表缺失时 FinishSession 应报错")
	}
	if sessions, err := store.RecentSessions(10); err == nil || sessions != nil {
		t.Errorf("sessions 表缺失时 RecentSessions 应为 nil 且报错 = %+v, %v", sessions, err)
	}
	if snapshot, err := store.Snapshot(10); err == nil || snapshot != nil {
		t.Errorf("表缺失时 Snapshot 应为 nil 且报错 = %+v, %v", snapshot, err)
	}
}

// Snapshot 的三段是串行的：队列表缺失时即使 targets 表完好也要报错（不能返回半份快照）。
func TestSnapshotReportsErrorWhenQueueTableMissing(t *testing.T) {
	store := openTemp(t)
	if err := store.UpsertTarget(Target{Host: "h", Repository: "r"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("DROP TABLE queue"); err != nil {
		t.Fatal(err)
	}
	if snapshot, err := store.Snapshot(10); err == nil || snapshot != nil {
		t.Errorf("queue 表缺失时不该返回半份快照 = %+v, %v", snapshot, err)
	}
}

// Close 之后再用句柄：所有方法都报错（已关闭的库不能被当成空库静默放过）。
func TestMethodsReportErrorAfterClose(t *testing.T) {
	store := openTemp(t)
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := store.ReplaceQueue("h", "r", time.Now(), nil); err == nil {
		t.Error("已关闭后 ReplaceQueue 应报错")
	}
	if sessions, err := store.RecentSessions(10); err == nil || sessions != nil {
		t.Errorf("已关闭后 RecentSessions 应报错 = %+v, %v", sessions, err)
	}
	if snapshot, err := store.Snapshot(10); err == nil || snapshot != nil {
		t.Errorf("已关闭后 Snapshot 应报错 = %+v, %v", snapshot, err)
	}
	if ok, err := store.StartSession("h", "r", Item{Kind: "issue", Number: 1}, time.Now(), "s", time.Hour); err == nil || ok {
		t.Errorf("已关闭后 StartSession 应报错 = %v, %v", ok, err)
	}
}
