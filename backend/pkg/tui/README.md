# DSHX TUI（pkg/tui）架构与手测指南

## 架构一览

```
stdin 字节流
  └─ startKeyPump (app.go)         唯一 stdin 读取者；ESC 消歧定时器（30ms）
       └─ keyParser (keys.go)      VT 序列 / UTF-8 / 括号粘贴 → keyEvent（纯函数，可单测）
            └─ Editor (editor.go)  多行缓冲 + 光标 + 历史 + 补全状态机（纯函数，可单测）
                 └─ presenter (screen.go) ─┐
stream 文本 → formatEnvelope → pres.Write ──┤ 帧通道（串行化）
                                            ▼
                                    Screen.loop（唯一 stdout 重绘者）
                                    [流式尾部] / [补全弹窗] / [状态栏] / [输入行]
```

- **底部区域不变式**：区域第一行要么为空、要么恰是未换行的流式尾部；从当前光标向上逐行擦除后，光标恰好落在下一次流式输出的追加点。因此流式输出与常驻输入区互不打断。
- **数据通路**：`tunedAdapter`（effortmeter.go）包裹真实 `llm.LlmAdapter` 后注入 agent——普通对话请求覆写 `ReasoningEffort`/`Model`，并从 usage chunk 累计 token；辅助请求（session-title/compaction）原样透传。Esc 中断走 `Agent.AbortTurn()`。
- **降级路径**：stdin 非 TTY（管道）或平台不支持 raw 模式时，走 `runCookedTUI`——改造前的 cooked 行扫描行为，逐字节保留。

## 键位表

| 按键 | 行为 |
|---|---|
| `Enter` | 提交；弹窗打开时改为接受高亮候选 |
| `Ctrl+J` / `Alt+Enter` | 插入换行（多行输入） |
| `/` 开头输入 | 实时弹出命令补全（共享注册表 + TUI 本地命令），前缀过滤+加粗高亮 |
| `↑` / `↓` | 弹窗：选择候选；多行：可视行移动；首/末行：历史导航 |
| `Ctrl+P` / `Ctrl+N` | 直接翻历史（无歧义备选键） |
| `Tab` / `Shift+Tab` | 接受候选 / 反向循环候选 |
| `Esc` | 关弹窗 → 中断当前回答（AbortTurn）→ 清空输入 |
| `Ctrl+C` | 第一击：清空输入行/关补全弹窗，状态栏提示 2 秒内再按退出；2 秒内第二击退出。审批等待中等价 `c`（仅取消审批，不退出） |
| `Home/End` `Ctrl+A/E/K/U/W` `Alt+B/F` `Alt+BS` | 行首/行尾、行内删除等 readline 编辑 |
| 粘贴多行文本 | 括号粘贴模式整体插入，不触发提交 |

本地命令：`/thinking [off|low|high|max]`（无参数循环切换）、`/model [id]`。二者刻意不进共享注册表（GUI/gateway 不可见）。

## 手测步骤（Windows Terminal / Windows 10+ conhost）

前置：`dshx tui`（或 `go run ./cmd/dsh tui`），需配置可用的 LLM 路由。

1. **启动态**：进入后应看到横幅（含 `model:` 当前模型与 `cwd:` 工作目录两行）+ 底部两行区（状态栏 + `> ` 输入行）。状态栏依次显示模型名 │ think:high │ cache – · 0 tok │ ○ ready。窄窗口（<40 列）时右侧段依次消失且永不换行。
2. **补全**：输入 `/pl` → 弹窗列出 plan（及匹配项），选中行反显、命中前缀加粗，底部有按键提示行；↑↓ 移动不干扰已输出内容；Tab 或 Enter 接受成 `/plan `；再次 Enter 提交。输入空格后弹窗关闭。
3. **历史**：提交几条消息后，空输入按 ↑ 逐条回溯、↓ 返回，回到最底恢复正在编辑的草稿；Ctrl+P/Ctrl+N 等效。重启 `dshx tui` 后 ↑ 仍能翻到上次会话的输入（落盘于 `%AppData%\dshx\tui-history.txt`）。
4. **多行**：Alt+Enter（或 Ctrl+J）插入换行，续行前缀 `· `；↑↓ 在行间移动光标而非翻历史；粘贴含换行的段落应整体进入输入框。
5. **思考等级**：`/thinking` 循环切换 low→high→max→off，状态栏即时变色；`/thinking off` 直设。发一条消息确认生效（off 时无灰色 reasoning 流）。
6. **模型名**：状态栏常显启动模型；`/model <其它id>` 切换后状态栏即时更新，下一条消息走新模型。
7. **缓存率**：连续对话第二轮起，cache 百分比上升、总 tokens 增长；usage 到达时状态栏即时刷新。
8. **流式中断**：流式输出期间按 Esc → 输出停止并出现 `[Turn Completed]`（aborted），输入区完好；再按 Esc 清空草稿。
9. **审批**：触发一个需批准的工具（如写文件）→ 底部提示符变 `? `，y/n/c/编号均可用；Esc 与 Ctrl+C 等价 c（仅取消审批，程序不退出）；无效输入给出提示且不崩溃。
10. **退出**：Ctrl+C 第一击清行并提示「再按一次 Ctrl+C 退出」，2 秒内第二击才退出；`/exit`、`/quit` 直接退出。终端模式/代码页恢复正常，无残留转义码。

已知限制（记录在案）：窗口宽度剧变后偶发一行渲染残影（重绘计数基于旧宽度）；传统 conhost 非 UTF-8 代码页下中文输入依赖启动时的 CP65001 切换；Shift+Enter 因各终端无统一序列而不做绑定。
