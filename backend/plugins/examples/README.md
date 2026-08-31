# 示例插件：echo-plugin

一个最小的外部（子进程）插件样例（plan Phase 4「真实可运行 examples」）。

## 契约

echo 走与内置外部插件相同的 JSON-RPC 握手（host.go 注释块）：

```text
Host → capability/initialize → {protocolVersion, serverName}
Host → capability/list       → CapabilitySpec[]
Host → tool/register  "<name>"  → ToolDefinition
Host → command/register "<name>" → 命令定义
Host → tool/call      {name, arguments} → 结果
Host → command/execute {name, rawArgs} → {kind, text}
子进程 → notifications/event/<topic> → 事件发布
```

`echo` 进程需实现以上方法的极小子集（initialize / capability/list /
tool/register / command/register / tool/call / command/execute）。

## 安装与验收路径

1. 把本目录 JSON 复制到插件根（默认由 `--plugin-dir` 指定），可执行文件
   `echo-plugin` 放在同目录；
2. 启动 dshx：`Reconcile` 拉起子进程 → 握手 → 工具/命令整代同步；
3. 验收点（对应 plan 验收 §7）：
   - `plugin.list` 显示 `echo-plugin`（source=external, status=mounted）及
     其工具/命令与 owner；
   - 会话内调用 `echo` 工具与 `/echo` 命令走真实 JSON-RPC 往返；
   - `plugins.Unload("echo-plugin")` 后工具/命令消失且可恢复；
   - kill 子进程 → 指数退避重连 → 整代重同步；
   - 删除 `.disabled` 外加名（持久化停用）后 Reconcile 不再拉起。

## 文件

- `echo-plugin.json` — manifest（ABI 1，tool+command 能力，重连策略）
- `echo-plugin` — 宿主实现的参考可执行（任意语言；方法集见
  `backend/pkg/plugin/host.go` 顶部协议注释）。