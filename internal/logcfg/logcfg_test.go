package logcfg

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// 固定时刻：UTC 渲染应恰好落在毫秒精度边界上（.999 不得进位或截断），
// 非 UTC 输入用来证明 write 会先转成 UTC。
var (
	fixedUTC = time.Date(2026, 9, 26, 1, 2, 3, 999_000_000, time.UTC)
	fixedCST = time.Date(2026, 9, 26, 9, 2, 3, 999_000_000, time.FixedZone("CST", 8*3600))
)

// 构造一个时钟钉死的 Logger，隔离时间戳对断言的干扰。
func testLogger(buffer *bytes.Buffer, verbose, debug bool, now time.Time) *Logger {
	logger := New(buffer, "assistant", verbose, debug)
	logger.clock = func() time.Time { return now }
	return logger
}

// 三级开关的真值表：debug 蕴含 verbose 是硬约定，不能只靠 --debug 单独生效。
func TestNewLevelMatrix(t *testing.T) {
	tests := []struct {
		name    string
		verbose bool
		debug   bool
		want    [3]bool // 依次为 Info / Verbose / Debug 是否放行
	}{
		{name: "默认", verbose: false, debug: false, want: [3]bool{true, false, false}},
		{name: "仅 verbose", verbose: true, debug: false, want: [3]bool{true, true, false}},
		{name: "仅 debug", verbose: false, debug: true, want: [3]bool{true, true, true}},
		{name: "两者都开", verbose: true, debug: true, want: [3]bool{true, true, true}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logger := New(&bytes.Buffer{}, "assistant", test.verbose, test.debug)
			if logger.verbose != test.want[1] {
				t.Errorf("verbose 字段 = %v, want %v（debug 蕴含 verbose）", logger.verbose, test.want[1])
			}
			for index, level := range []Level{LevelInfo, LevelVerbose, LevelDebug} {
				if got := logger.Enabled(level); got != test.want[index] {
					t.Errorf("Enabled(%v) = %v, want %v", level, got, test.want[index])
				}
			}
		})
	}
}

// 未知级别一律不放行：Enabled 的零值分支不能意外返回 true。
func TestEnabledUnknownLevel(t *testing.T) {
	logger := New(&bytes.Buffer{}, "assistant", true, true)
	for _, level := range []Level{Level(-1), Level(3), Level(42)} {
		if logger.Enabled(level) {
			t.Errorf("Enabled(%d) 应为 false（未定义级别）", level)
		}
	}
}

// nil 不是错误：命令层常持有可能未注入的 *Logger，所有方法都必须静默降级。
func TestNilLoggerSafety(t *testing.T) {
	var logger *Logger

	tests := []struct {
		name string
		call func()
	}{
		{name: "Infof", call: func() { logger.Infof("x=%d", 1) }},
		{name: "Verbosef", call: func() { logger.Verbosef("x=%d", 1) }},
		{name: "Debugf", call: func() { logger.Debugf("x=%d", 1) }},
		{name: "Warnf", call: func() { logger.Warnf("x=%d", 1) }},
		{name: "write", call: func() { logger.write(LevelInfo, "x=%d", 1) }},
		{name: "ToFunc 回调", call: func() { logger.ToFunc(LevelInfo)("x") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// 任何一次 panic 都会让该子测试失败，无需额外断言。
			test.call()
		})
	}

	if logger.Enabled(LevelInfo) {
		t.Error("nil Logger 的 Info 级也不应放行")
	}
	if logger.Enabled(LevelDebug) {
		t.Error("nil Logger 的 Debug 级也不应放行")
	}
}

// 各级方法的输出与否只由自己的级别开关决定，且格式统一。
func TestLevelGatingAndFormat(t *testing.T) {
	tests := []struct {
		name    string
		verbose bool
		debug   bool
		call    func(*Logger)
		want    string
	}{
		{
			name: "默认只出 Info",
			call: func(logger *Logger) {
				logger.Infof("状态: %s", "完成")
				logger.Verbosef("步骤: %s", "拉取")
				logger.Debugf("决策: %s", "重试")
				logger.Warnf("警告: %s", "超时")
			},
			want: "[assistant 2026-09-26T01:02:03.999Z] 状态: 完成\n" +
				"[assistant 2026-09-26T01:02:03.999Z] 警告: 超时\n",
		},
		{
			name:    "verbose 加放 Verbosef",
			verbose: true,
			call: func(logger *Logger) {
				logger.Verbosef("步骤: %s", "拉取")
				logger.Debugf("决策: %s", "重试")
			},
			want: "[assistant 2026-09-26T01:02:03.999Z] 步骤: 拉取\n",
		},
		{
			name:    "仅 debug 旗标也放行 verbose",
			verbose: false,
			debug:   true,
			call: func(logger *Logger) {
				logger.Verbosef("步骤: %s", "拉取")
				logger.Debugf("决策: %s", "重试")
				logger.Infof("结果: %d", 0)
			},
			want: "[assistant 2026-09-26T01:02:03.999Z] 步骤: 拉取\n" +
				"[assistant 2026-09-26T01:02:03.999Z] 决策: 重试\n" +
				"[assistant 2026-09-26T01:02:03.999Z] 结果: 0\n",
		},
		{
			name: "未启用级别一字不出",
			call: func(logger *Logger) {
				logger.Verbosef("步骤: %s", "拉取")
				logger.Debugf("决策: %s", "重试")
			},
			want: "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var buffer bytes.Buffer
			logger := testLogger(&buffer, test.verbose, test.debug, fixedUTC)
			test.call(logger)
			if got := buffer.String(); got != test.want {
				t.Errorf("输出 = %q, want %q", got, test.want)
			}
		})
	}
}

// 时间戳在 write 边界统一渲染为 UTC 的毫秒精度，非 UTC 输入必须被换算。
func TestWriteRendersUTCTimestamp(t *testing.T) {
	tests := []struct {
		name string
		now  time.Time
		want string
	}{
		{name: "UTC 输入直接渲染", now: fixedUTC, want: "[bot 2026-09-26T01:02:03.999Z] hello\n"},
		{name: "东八区输入换算为 UTC", now: fixedCST, want: "[bot 2026-09-26T01:02:03.999Z] hello\n"},
		{name: "跨日回退一天", now: time.Date(2026, 1, 1, 3, 0, 0, 0, time.FixedZone("UTC+3", 3*3600)), want: "[bot 2026-01-01T00:00:00.000Z] hello\n"},
		{name: "零值时间", now: time.Time{}, want: "[bot 0001-01-01T00:00:00.000Z] hello\n"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var buffer bytes.Buffer
			logger := testLogger(&buffer, false, false, test.now)
			logger.prefix = "bot"
			logger.Infof("%s", "hello")
			if got := buffer.String(); got != test.want {
				t.Errorf("输出 = %q, want %q", got, test.want)
			}
		})
	}
}

// ToFunc 折出的回调走与直接调用同一套级别判定。
func TestToFuncLevels(t *testing.T) {
	tests := []struct {
		name    string
		verbose bool
		debug   bool
		level   Level
		want    string
	}{
		{name: "Info 回调恒输出", level: LevelInfo, want: "[assistant 2026-09-26T01:02:03.999Z] line\n"},
		{name: "Verbose 回调默认被抑制", level: LevelVerbose, want: ""},
		{name: "Verbose 回调随旗标放行", verbose: true, level: LevelVerbose, want: "[assistant 2026-09-26T01:02:03.999Z] line\n"},
		{name: "Debug 回调随旗标放行", debug: true, level: LevelDebug, want: "[assistant 2026-09-26T01:02:03.999Z] line\n"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var buffer bytes.Buffer
			logger := testLogger(&buffer, test.verbose, test.debug, fixedUTC)
			logger.ToFunc(test.level)("line")
			if got := buffer.String(); got != test.want {
				t.Errorf("输出 = %q, want %q", got, test.want)
			}
		})
	}
}

// 并发安全：多 goroutine 混写不得出现撕裂或交错的记录行。
func TestConcurrentWritesStayIntact(t *testing.T) {
	const (
		writers  = 8
		perWrite = 40
	)

	var buffer bytes.Buffer
	logger := testLogger(&buffer, true, true, fixedUTC)

	var wait sync.WaitGroup
	for writer := range writers {
		wait.Go(func() {
			for index := range perWrite {
				logger.Infof("线程%d-序号%d", writer, index)
				logger.Verbosef("步骤%d-%d", writer, index)
				logger.Debugf("决策%d-%d", writer, index)
				logger.Warnf("警告%d-%d", writer, index)
			}
		})
	}
	wait.Wait()

	lines := strings.Split(strings.TrimSuffix(buffer.String(), "\n"), "\n")
	wantCount := writers * perWrite * 4
	if len(lines) != wantCount {
		t.Fatalf("应输出 %d 行完整记录，实际 %d 行", wantCount, len(lines))
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "[assistant 2026-09-26T01:02:03.999Z] ") {
			t.Fatalf("记录行前缀被破坏：%q", line)
		}
		body := strings.TrimPrefix(line, "[assistant 2026-09-26T01:02:03.999Z] ")
		if !isWellFormedBody(body) {
			t.Fatalf("记录行正文被破坏（疑似交错）：%q", body)
		}
	}
}

// isWellFormedBody 校验正文形如「标记<数字>-<类型><数字>」：标记是一种日志
// 类型，如「线程7-序号0」。任何交错、重复或被截断的行都会在这里露馅。
func isWellFormedBody(body string) bool {
	for _, marker := range []string{"线程", "步骤", "决策", "警告"} {
		rest, ok := strings.CutPrefix(body, marker)
		if !ok {
			continue
		}
		writer, item, found := strings.Cut(rest, "-")
		if !found {
			return false
		}
		return isNumberedItem(writer) && isNumberedItem(item)
	}
	return false
}

// isNumberedItem 报告片段是否形如「<非数字前缀><纯数字后缀>」，且数字部分非空。
func isNumberedItem(segment string) bool {
	digits := 0
	for _, char := range segment {
		switch {
		case char >= '0' && char <= '9':
			digits++
		case digits > 0:
			// 数字之后再出现非数字字符：说明这一行被别的写入插了进来。
			return false
		}
	}
	return digits > 0
}
