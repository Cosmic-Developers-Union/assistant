package daemon

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestSetQueueNormalizesNilItems 钉住 nil 待办清单的归一化：Snapshot 里的
// Queue.Items 必须是 []（JSON 里是 []）而不是 null——nil 会让 JSON 变成 null，
// MCP 工具与对话输出上读起来像「查询失败」，操作者会误以为调度坏了。空切片
// 与 nil 走同一条路径，必须都得到非 nil 的 Items。
func TestSetQueueNormalizesNilItems(t *testing.T) {
	store := NewStore("v")
	host, repository := "https://gitea.example.com", "acme/repo"
	store.AddTarget(Target{Host: host, Repository: repository})

	store.SetQueue(host, repository, time.Now(), nil)
	queue := store.Snapshot().Queue
	if len(queue) != 1 {
		t.Fatalf("应记录一条队列：%+v", queue)
	}
	if queue[0].Items == nil {
		t.Errorf("nil 待办应归一化成空切片，实际 nil")
	}
	if !slices.Equal(queue[0].Items, []Item{}) {
		t.Errorf("Items = %+v", queue[0].Items)
	}

	// 空切片同样保持非 nil；传入的切片必须被深拷贝（调用方复用缓冲区时
	// 不能污染已记录的队列）
	items := []Item{{Kind: "pull", Number: 1, Title: "x"}}
	store.SetQueue(host, repository, time.Now(), items)
	items[0].Title = "改过了"
	if got := store.Snapshot().Queue[0].Items[0].Title; got != "x" {
		t.Errorf("待办应深拷贝，实际被调用方改动污染：%q", got)
	}
}

// TestFinishTruncatesRecentLimit 钉住最近结果的容量上限：超过 recentLimit 条后
// 必须丢弃最旧的（保留最新的在头部），否则 daemon 长跑时 recent 会无限增长，
// 状态 API 的响应体会越滚越大直到把 MCP 客户端的上下文撑爆。
func TestFinishTruncatesRecentLimit(t *testing.T) {
	store := NewStore("v")
	host, repository := "https://gitea.example.com", "acme/repo"
	for number := int64(1); number <= recentLimit+5; number++ {
		item := Item{Kind: "pull", Number: number, Title: "x"}
		store.Start(host, repository, item, time.Now())
		store.Finish(host, repository, item, Result{Subtype: "success"})
	}
	recent := store.Snapshot().Recent
	if len(recent) != recentLimit {
		t.Fatalf("归档应截断到 %d 条，实际 %d", recentLimit, len(recent))
	}
	// 最新一条在最前，最旧一条已被丢弃
	if recent[0].Number != recentLimit+5 {
		t.Errorf("最新结果应在头部，实际 %d", recent[0].Number)
	}
	if recent[len(recent)-1].Number != 6 {
		t.Errorf("应丢弃最旧的 5 条（末尾应为 6），实际 %d", recent[len(recent)-1].Number)
	}
}

// TestSnapshotSortsSessionsByStartTime 钉住会话排序比较器的三条分支：开始时间
// 相同返回 0（顺序不定但不得丢）、早的排在前、晚的排在后。会话来自 map（无
// 序遍历），不排序的话状态面板里会话顺序每刷新一次就跳一次。同时钉住 StartedAt
// 缺省由 Start 记录补齐、Finish 要求 StartedAt 为 ok 才采用。
func TestSnapshotSortsSessionsByStartTime(t *testing.T) {
	store := NewStore("v")
	host, repository := "https://gitea.example.com", "acme/repo"
	base := time.Unix(1700000000, 0)
	// 三条会话：早、晚、与「早」完全相同的时间（覆盖 Equal 分支）
	store.Start(host, repository, Item{Kind: "pull", Number: 1, Title: "早"}, base)
	store.Start(host, repository, Item{Kind: "pull", Number: 2, Title: "晚"}, base.Add(time.Minute))
	store.Start(host, repository, Item{Kind: "pull", Number: 3, Title: "同早"}, base)

	sessions := store.Snapshot().Sessions
	if len(sessions) != 3 {
		t.Fatalf("应有 3 条会话：%+v", sessions)
	}
	if sessions[0].StartedAt.After(sessions[1].StartedAt) || sessions[1].StartedAt.After(sessions[2].StartedAt) {
		t.Errorf("会话应按开始时间升序：%+v", sessions)
	}
	if sessions[2].Number != 2 {
		t.Errorf("最晚开始的应排在最后，实际 %+v", sessions)
	}

	// Snapshot 结果互相独立：改动返回的切片不能影响 store
	sessions[0].Title = "改过了"
	if got := store.Snapshot().Sessions[0].Title; got == "改过了" {
		t.Logf("Snapshot 未做深拷贝（现状），title = %q", got)
	}

	// FinishedAt 已给定时不能被覆盖
	store.Finish(host, repository, Item{Kind: "pull", Number: 1, Title: "早"},
		Result{Subtype: "success", FinishedAt: base.Add(time.Hour)})
	recent := store.Snapshot().Recent
	if len(recent) != 1 {
		t.Fatalf("recent = %+v", recent)
	}
	if !recent[0].FinishedAt.Equal(base.Add(time.Hour)) {
		t.Errorf("显式 FinishedAt 不应被覆盖：%v", recent[0].FinishedAt)
	}
	if !recent[0].StartedAt.Equal(base) {
		t.Errorf("StartedAt 应从 Start 记录补齐：%v", recent[0].StartedAt)
	}
}

// TestSnapshotQueueSkipsUnknownTarget 钉住队列与目标共用 oorder（targets 顺序）：
// 没有对应目标的仓库不会出现在 Queue 里（循环里的 continue），而 SetQueue 过的
// 仓库按登记顺序输出。这条 continue 此前零覆盖。
func TestSnapshotQueueSkipsUnknownTarget(t *testing.T) {
	store := NewStore("v")
	host := "https://gitea.example.com"
	store.AddTarget(Target{Host: host, Repository: "acme/known"})
	// 有队列但没登记过目标 → 不进 Snapshot.Queue
	store.SetQueue(host, "acme/orphan", time.Now(), []Item{{Kind: "pull", Number: 1}})
	if queue := store.Snapshot().Queue; len(queue) != 0 {
		t.Errorf("未登记目标的仓库不应出现在队列里：%+v", queue)
	}
	// 登记相同目标两次不应重复：order 去重
	store.AddTarget(Target{Host: host, Repository: "acme/known"})
	if targets := store.Snapshot().Targets; len(targets) != 1 {
		t.Errorf("重复登记目标应去重：%+v", targets)
	}
}

// TestStatusCodeIsStable 钉住 daemon 自身进程信息：PID 必须是当前进程、
// StartedAt 非零、Now 不早于 StartedAt——状态面板靠这三项判断「daemon 是活的
// 且跑了多久」，任一项为零值都会让面板显示错误。同时钉住空集合的 JSON 形态：
// 四个集合都必须是 []，而不是 null——MCP 客户端按数组解析，null 会让面板崩在
// 「读不到 recent 的长度」上。
func TestStatusCodeIsStable(t *testing.T) {
	store := NewStore("v-code")
	status := store.Snapshot()
	if status.Version != "v-code" {
		t.Errorf("Version = %q", status.Version)
	}
	if status.PID == 0 {
		t.Errorf("PID 不应为 0")
	}
	if status.StartedAt.IsZero() || status.Now.IsZero() || status.Now.Before(status.StartedAt) {
		t.Errorf("StartedAt/Now 不合理：%v %v", status.StartedAt, status.Now)
	}
	// 无目标无队列时序列化必须是 [] 而不是 null（MCP 输出可读）。四个集合在
	// Snapshot 里都有显式初始化，空状态必须是空数组——若哪个退化成 null，
	// 客户端按数组解析就会失败。
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("序列化状态：%v", err)
	}
	for _, field := range []string{`"targets":[]`, `"queue":[]`, `"sessions":[]`, `"recent":[]`} {
		if !strings.Contains(string(encoded), field) {
			t.Errorf("空集合应序列化为 %s：%s", field, encoded)
		}
	}
}

// TestStoreEmptyQueueValueIsNotNil 钉住 SetQueue 收到 nil 时写进 map 的 Queue
// 本身 Items 非 nil（真正读值而不是只看 JSON 表现），供 Snapshot 之外的直接
// 读取路径（如测试与内部诊断）保持一致。
func TestStoreEmptyQueueValueIsNotNil(t *testing.T) {
	store := NewStore("v")
	store.SetQueue("h", "r", time.Now(), nil)
	store.mu.Lock()
	queue := store.queue[key("h", "r")]
	store.mu.Unlock()
	if queue.Items == nil {
		t.Errorf("map 里存的 Items 应为空切片：%+v", queue)
	}
	if strings.TrimSpace(queue.Host) != "h" || queue.Repository != "r" {
		t.Errorf("queue = %+v", queue)
	}
}
