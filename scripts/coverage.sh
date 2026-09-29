#!/usr/bin/env bash
# 测试覆盖率门禁：按包对照阈值，任一不达标即非零退出。
#
# 阈值分两档（见 AGENTS.md「测试与覆盖率」）：
#   - 核心（core）：90%——承载系统语义的包（评审状态机、调度引擎、配置装载、
#     凭据、存储、setup、provider 等）。判定依据是「是否定义系统行为」而非
#     「代码量」。
#   - 一般（general）：80%——I/O 适配器与胶水（各消息通道、MCP 桥接、
#     脚手架生成等）。
#
# 覆盖率是必要条件而非充分条件：公共 API、错误路径、边界条件与协议/接口契约
# 必须有测试，不受本脚本的数字影响（数字达标但契约无测试同样不合格）。
#
# 用法：
#   scripts/coverage.sh              # 跑测试并对照阈值
#   scripts/coverage.sh --report     # 只打印，不因不达标而失败（用于摸底）

set -euo pipefail

# 只读缓存环境（CI 与只读 ~/.cache/go-build 的机器）下也能跑
export GOCACHE="${GOCACHE:-/tmp/assistant-gocache}"
export GOTMPDIR="${GOTMPDIR:-/tmp}"

profile="$(mktemp -t assistant-cover.XXXXXX)"
trap 'rm -f "$profile"' EXIT

report_only=false
if [[ "${1:-}" == "--report" ]]; then
	report_only=true
fi

# 阈值表：<包路径后缀>=<阈值>。核心 90，一般 80。
# 新增包时必须在此登记，否则脚本报错（防止漏管）。
declare -A thresholds=(
	[internal/status]=90
	[internal/dispatcher]=90
	[internal/daemon]=90
	[internal/instances]=90
	[internal/config]=90
	[internal/credentials]=90
	[internal/sessionstore]=90
	[internal/sessionindex]=90
	[internal/statestore]=90
	[internal/setup]=90
	[internal/provider]=90
	[internal/claudecfg]=90
	[internal/claude]=90
	[internal/integration]=90
	[cmd/assistant]=90
	[internal/integration/gitea]=90
	[internal/integration/weixin]=80
	[internal/integration/qq]=80
	[internal/integration/telegram]=80
	[internal/mcps]=80
	[internal/repoinstall]=80
	[internal/runcfg]=80
	[internal/conversations]=80
	[internal/envref]=80
	[internal/agents]=80
	[internal/logcfg]=80
	[skills]=80
)

# asset-only：纯 //go:embed 资源持有者，没有可执行语句（覆盖率对它无意义）。
# 登记在此而不是默默忽略——新增包若既不在阈值表也不在此，脚本会报错要求表态。
declare -A asset_only=(
	[content]=1
	[schema]=1
	# 测试替身包：只被 _test.go 引用，没有生产代码（覆盖率对它无意义）
	[internal/integration/qq/qqtestsupport]=1
)

module="$(go list -m)"

echo "==> 运行测试并采集覆盖率..."
go test ./... -coverprofile="$profile" -covermode=atomic >/tmp/assistant-cover-test.log 2>&1 || {
	echo "!! 测试失败（覆盖率门禁要求测试全绿）：" >&2
	tail -40 /tmp/assistant-cover-test.log >&2
	exit 1
}

echo "==> 按包核对阈值（核心 90% / 一般 80%）"
printf '%-38s %8s %6s %8s  %s\n' "包" "覆盖率" "阈值" "差距" "状态"

# 按语句加权统计每个包的覆盖率（与 go test 的 "coverage: X% of statements" 同口径）：
# profile 每行是 <文件>:<范围> <语句数> <命中次数>，命中计入分子、语句数计入分母。
# 不能用 `go tool cover -func` 的逐函数百分比求平均——那是函数等权，与语句加权不等价。
package_coverage() {
	awk '
		NR == 1 { next }                       # 跳过 mode: 行
		{
			file = $1
			sub(/:[0-9].*$/, "", file)          # 去掉 :行.列,行.列
			split(file, parts, "/")
			pkg = file
			sub(/\/[^\/]+$/, "", pkg)           # 去掉文件名，留下包目录
			statements = $2 + 0
			hits = $3 + 0
			total[pkg] += statements
			if (hits > 0) covered[pkg] += statements
		}
		END {
			for (pkg in total) {
				if (total[pkg] > 0) {
					printf "%s\t%.1f\n", pkg, 100 * covered[pkg] / total[pkg]
				}
			}
		}' "$profile"
}

failed=0
while IFS=$'\t' read -r pkg pct; do
	suffix="${pkg#"$module"/}"
	if [[ -n "${asset_only[$suffix]:-}" ]]; then
		continue
	fi
	threshold="${thresholds[$suffix]:-}"
	if [[ -z "$threshold" ]]; then
		printf '%-38s %7s%% %6s %8s  %s\n' "$suffix" "$pct" "-" "-" "未登记阈值"
		echo "   → 新增包必须在 scripts/coverage.sh 的 thresholds（有逻辑）或 asset_only（纯资源）中登记" >&2
		failed=1
		continue
	fi

	whole="${pct%%.*}"
	gap=$((whole - threshold))
	status="OK"
	if ((gap < 0)); then
		status="不达标"
		failed=1
	fi
	printf '%-38s %7s%% %6s %8s  %s\n' "$suffix" "$pct" "${threshold}%" "${gap}%" "$status"
done < <(package_coverage | sort)

# 阈值表登记了但 profile 里一个语句都没有的包（没有测试文件）必须报错：
# 「没跑测试」与「覆盖率为 0」是两件事，不能靠数字为 0 蒙混过去。
for suffix in "${!thresholds[@]}"; do
	if ! go test -count=1 -cover "$module/$suffix" >/dev/null 2>&1; then
		echo "   $suffix: 测试未通过或不存在（阈值表已登记，必须有测试）" >&2
		failed=1
	fi
done

echo
echo "==> 实测总覆盖率（含未登记的包，仅供参考）"
go tool cover -func="$profile" | tail -1

if ((failed)); then
	echo
	if $report_only; then
		echo "==> 有未达标项（--report 模式：不作为失败退出）" >&2
		exit 0
	fi
	echo "!! 覆盖率门禁未通过：见上表的「不达标」与「未登记阈值」项" >&2
	echo "   说明：覆盖率是必要条件；公共 API、错误路径、边界条件与协议契约" >&2
	echo "   必须有测试，不受数字影响。" >&2
	exit 1
fi

echo
echo "==> 覆盖率门禁通过（核心 90% / 一般 80%）"
