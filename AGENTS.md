use `https://gitea.com/gitea/go-sdk` with gitea
XDG Base Directory Specification / Windows Known Folders / macOS Library Directory conventions

日志必须让操作者能够观察系统状态、关键事件和故障原因；`--verbose` 增加正常运行细节，`--debug` 进一步暴露内部诊断信息
更具体地说：

- `默认`：只输出操作者真正需要知道的状态、结果、警告和错误
  -`--verbose`：展示“系统正在做什么”，例如步骤、目标、进度、外部调用和状态变化
  -`--debug`：展示“系统为什么这样做”，例如内部决策、参数、分支、重试、底层错误和诊断上下文

可以简单理解为：

```text
default   = What happened?
verbose   = What is happening?
debug     = Why is it happening?
```