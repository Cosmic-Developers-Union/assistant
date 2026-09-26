package main

import (
	"strings"
	"testing"
)

// hugeDigits 造一个必然让 strconv.ParseFloat 回 ErrRange 的十进制数字串。
//
// 位数取得足够多（400 位）是刻意的：float64 的最大值约 1.8e308，任何远超它的
// 十进制字面量都会让 ParseFloat 回 "value out of range" 而不是一个 Inf。这正是
// parseDayDuration 里 d/w 分支必须自己接住的那个错误。
func hugeDigits() string {
	return strings.Repeat("9", 400)
}

// TestParseDayDurationDayArmReportsUnparsableNumber 断言 d/w 分支里数字本身大到
// 无法表示时报错，而不是把 ParseFloat 的错误丢掉继续算。
//
// 这不是理论分支：输入是用户敲在命令行上的，手抖多摁住一个键就会得到几十位数字。
// 这条错误路径此前没有任何用例走过（覆盖率剖面里 61.5,62.1 长期为 0），于是
// 「吞掉 ParseFloat 错误拿 0 继续」这种回归不会被任何测试拦住——而它的表现是
// 一个巨大的时长被静默折成 0，用户看到的是「等待时间已设为 0」。
func TestParseDayDurationDayArmReportsUnparsableNumber(t *testing.T) {
	huge := hugeDigits() + "d"
	_, err := parseDayDuration(huge)
	if err == nil {
		t.Fatal("数字超出 float64 可表示范围时应报错")
	}
	if !strings.Contains(err.Error(), "无法解析时长") {
		t.Errorf("错误应点明无法解析时长：%v", err)
	}
	// 必须透出 ParseFloat 的原始错误：只有它能告诉操作者问题在数字大小上，
	// 而 parseDayDuration 自己的包装文案（无效片段/超出可表示范围）都不适用。
	if !strings.Contains(err.Error(), "value out of range") {
		t.Errorf("应保留底层 ParseFloat 的错误：%v", err)
	}

	// w 与 d 共用同一个分支，用不同单位再钉一遍，避免只测到其中一条 case 标签
	if _, err := parseDayDuration(hugeDigits() + "w"); err == nil {
		t.Error("w 单位的溢出同样应报错")
	}
}

// TestParseDayDurationStandardArmReportsUnparsableNumber 断言非 d/w 分支里
// time.ParseDuration 拒绝超长数字时报错，而不是当成 0 时长放行。
//
// 与 d 分支同理，但走的是另一条 return（覆盖率剖面里 74.5,75.1 长期为 0）。
// 两条分支各有一处错误处理，只测其中一条会让另一条的错误吞没悄悄溜回主干。
func TestParseDayDurationStandardArmReportsUnparsableNumber(t *testing.T) {
	for _, unit := range []string{"s", "h", "ms"} {
		_, err := parseDayDuration(hugeDigits() + unit)
		if err == nil {
			t.Fatalf("单位 %s 下数字溢出应报错", unit)
		}
		if !strings.Contains(err.Error(), "无法解析时长") {
			t.Errorf("单位 %s 的错误应点明无法解析时长：%v", unit, err)
		}
		if !strings.Contains(err.Error(), "invalid duration") {
			t.Errorf("单位 %s 应保留底层 time.ParseDuration 的错误：%v", unit, err)
		}
	}

	// 组合串：前缀合法片段之后的第二段溢出，确保错误来自第二段而不是第一段
	_, err := parseDayDuration("1d" + hugeDigits() + "s")
	if err == nil {
		t.Fatal("组合串中后段溢出应报错")
	}
	if !strings.Contains(err.Error(), "无法解析时长") {
		t.Errorf("组合串错误应点明无法解析时长：%v", err)
	}
}
