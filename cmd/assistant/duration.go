package main

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"time"
)

// dayDuration 是 pflag.Value 实现，在 time.ParseDuration 的单位之上扩展
// d（天）和 w（周），支持 1d1m1s、2w 这类组合写法。
type dayDuration struct {
	value *time.Duration
}

// durationToken 匹配一个“数值+单位”片段；单位表覆盖 time.ParseDuration
// 的全部单位并追加 d / w。
var durationToken = regexp.MustCompile(`^(\d+(?:\.\d+)?)(ns|us|µs|μs|ms|s|m|h|d|w)`)

// maxDurationDays 是 int64 纳秒能表示的最大天数；天数超过它之后 float→Duration
// 转换会静默饱和而不是报错。
const maxDurationDays = float64(math.MaxInt64) / float64(24*time.Hour)

func (d dayDuration) String() string {
	return d.value.String()
}

func (d dayDuration) Set(input string) error {
	parsed, err := parseDayDuration(input)
	if err != nil {
		return err
	}
	*d.value = parsed
	return nil
}

func (d dayDuration) Type() string {
	return "duration"
}

func parseDayDuration(input string) (time.Duration, error) {
	if input == "" {
		return 0, fmt.Errorf("时长不能为空")
	}
	if input == "0" {
		return 0, nil
	}
	var total time.Duration
	rest := input
	for len(rest) > 0 {
		match := durationToken.FindStringSubmatch(rest)
		if match == nil {
			return 0, fmt.Errorf("无法解析时长 %q：无效片段 %q（支持 ns/us/ms/s/m/h/d/w 组合，如 1d1m1s）", input, rest)
		}
		number, unit := match[1], match[2]
		var part time.Duration
		if unit == "d" || unit == "w" {
			days, err := strconv.ParseFloat(number, 64)
			if err != nil {
				return 0, fmt.Errorf("无法解析时长 %q: %w", input, err)
			}
			if unit == "w" {
				days *= 7
			}
			if days > maxDurationDays {
				return 0, fmt.Errorf("无法解析时长 %q：%s%s 超出可表示范围", input, number, unit)
			}
			part = time.Duration(days * float64(24*time.Hour))
		} else {
			var err error
			part, err = time.ParseDuration(number + unit)
			if err != nil {
				return 0, fmt.Errorf("无法解析时长 %q: %w", input, err)
			}
		}
		total += part
		if total < 0 {
			return 0, fmt.Errorf("无法解析时长 %q：总时长超出可表示范围", input)
		}
		rest = rest[len(match[0]):]
	}
	return total, nil
}
