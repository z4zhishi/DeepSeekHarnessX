extends "res://tests/probe_gui_shell_base.gd"

## GUI 发送→流式→工具行→切换会话 探针（Goal §3「GUI 壳」行的行为半边）。
##
## 全链路走真实产品路径：
##   发送：composer 真实 %Prompt 草稿 + %SendBtn pressed（composer 探针同款
##         触发法）→ app._on_prompt → session.prompt RPC。
##   下行：mock 侧经 DshClient._handle_downlink_message（真实 WS 帧解析）
##         投 server-request session/event → app._on_session_event →
##         ChatList.apply_event（ConversationFold）。
##   切换：app._on_session_picked("s2") → resume + history 回放真实路径。
##
## RESULT：GUI_SHELL_RESULT passed=N failed=M（shell / habits 两个探针同名标签，
## 各自独立进程输出，不混流）


func result_tag() -> String:
	return "GUI_SHELL_RESULT"


const TURN_EVENTS := [
	{"type": "turn/start", "seq": 1, "data": {"turn": 1}},
	{"type": "user/message", "seq": 2, "data": {"turn": 1, "message": {
		"id": "u1", "content": [{"type": "text", "text": "探针消息：你好 DSHX"}]}}},
	{"type": "assistant/chunk", "seq": 3, "data": {"turn": 1, "chunk": {"type": "text-delta", "text": "正在分析"}}},
	{"type": "assistant/chunk", "seq": 4, "data": {"turn": 1, "chunk": {"type": "text-delta", "text": " 工作区。"}}},
	{"type": "assistant/message", "seq": 5, "data": {"turn": 1, "message": {
		"id": "a1", "content": [{"type": "text", "text": "正在分析 工作区。"}]}}},
	{"type": "tool/call", "seq": 6, "data": {"turn": 1, "step": 2, "callId": "call-1",
		"name": "fs.read", "arguments": "{\"path\":\"README.md\"}"}},
	{"type": "tool/result", "seq": 7, "data": {"turn": 1, "callId": "call-1",
		"message": {"content": [{"type": "text", "text": "README 全文"}]}}},
	{"type": "turn/end", "seq": 8, "data": {"turn": 1, "reason": {"kind": "completed"}}},
]

# 第二会话（s2）的历史：切回时必须重放这条 user 气泡 + 旧 assistant 文本
const S2_HISTORY := [
	{"type": "user/message", "seq": 1, "data": {"turn": 1, "message": {
		"id": "u2", "content": [{"type": "text", "text": "s2 历史提问"}]}}},
	{"type": "assistant/chunk", "seq": 2, "data": {"turn": 1, "chunk": {"type": "text-delta", "text": "s2 历史回答"}}},
	{"type": "assistant/message", "seq": 3, "data": {"turn": 1, "message": {
		"id": "a2", "content": [{"type": "text", "text": "s2 历史回答"}]}}},
	{"type": "turn/end", "seq": 4, "data": {"turn": 1, "reason": {"kind": "completed"}}},
]


func _run() -> void:
	# histories 由探针持有引用：本轮 live turn 结束后写回 s1 的"持久化"历史，
	# （后端本就把事件落库，之后 session.history 必须能读回）。
	var histories := {"s2": S2_HISTORY.duplicate(true)}
	_histories = histories
	var loaded := await boot_app([
		{"id": "s1", "title": "RT 会话", "cwd": "C:/repo"},
		{"id": "s2", "title": "历史会话", "cwd": "C:/repo"},
	], histories, [])
	if not loaded:
		_assert(false, "app.tscn + mock 网关装载")
		return
	var sent := await _verify_send_turn()
	if not sent:
		return
	await _verify_session_switch()


var _histories: Dictionary = {}


## —— 发送 → 流式 → 工具行（事件序与 app.gd 处理逐点对照） ————————
func _verify_send_turn() -> bool:
	print("== 发送 → 流式 assistant → 工具行 ==")
	var composer: Node = app._composer
	var send_btn := composer.get_node("%SendBtn") as Button
	if send_btn == null:
		_assert(false, "%SendBtn 存在（真实发送路径入口）")
		return false
	# 发送前清点 rpc；发送经 composer 真实按钮（非直调 app._on_prompt）
	client_rpc_clear()
	composer.call("set_draft", "探针消息：你好 DSHX")
	send_btn.pressed.emit()
	_assert(str(composer.call("get_draft")) == "", "发送后 composer 草稿清空")
	var prompts := client_rpc_calls("session.prompt")
	_assert(prompts.size() == 1 and str((prompts[0] as Dictionary).get("payload", {}).get("text", "")) == "探针消息：你好 DSHX",
			"send press → session.prompt RPC（文本一致，app._on_prompt 真实链路）")
	_assert(bool(composer.call("is_generating")), "发送后进入 generating（watchdog 已武装）")
	_assert(app._hero.visible == false and app._chat.visible, "发送即离开空态（chat 可见）")
	# turn/start 单帧：只应翻 generating，不产生聊天行
	client.call("deliver_session_event", TURN_EVENTS[0])
	await _frames(2)
	_assert(bool(composer.call("is_generating")) and chat_nodes().is_empty(),
			"turn/start：generating=true 且聊天列表仍为空（事件未越权落卡）")
	for i in range(1, TURN_EVENTS.size()):
		client.call("deliver_session_event", TURN_EVENTS[i])
	await _frames(6)
	await _frames(2)  # 给 _sync 定时器一帧挂行
	var kinds := chat_kinds()
	var want := ["user", "assistant", "tool"]
	_assert(kinds == want, "折叠序 user → assistant → tool（实测 kinds=%s）" % str(kinds))
	if kinds != want:
		return false
	var nodes := chat_nodes()
	var user_payload: Dictionary = nodes[0]["payload"]
	_assert(str(user_payload.get("text", "")) == "探针消息：你好 DSHX",
			"user 气泡文案来自 user/message 帧")
	var asst_payload: Dictionary = nodes[1]["payload"]
	_assert(str(asst_payload.get("text", "")) == "正在分析 工作区。",
			"assistant 气泡聚合流式 chunk（chunk3+chunk4 == assistant/message 全文）")
	_assert(bool(asst_payload.get("streaming", true)) == false,
			"assistant/message / turn/end 后 streaming=false（流式闭合）")
	var tool_payload: Dictionary = nodes[2]["payload"]
	_assert(str(tool_payload_get_name(tool_payload)) == "fs.read" and str(tool_payload.get("status", "")) == "done",
			"工具行 kind=tool：fs.read 状态 running→done")
	_assert(str(tool_payload.get("output", "")) == "README 全文", "工具行收到 tool/result 输出")
	_assert(bool(composer.call("is_generating")) == false, "turn/end 后生成态归位")
	# 挂载证据：折叠节点真的落到 ChatList 活树上（virtual_list 行实例）
	var live: Variant = app._chat.get("_live")
	var live_ok := live is Dictionary and not (live as Dictionary).is_empty()
	_assert(live_ok, "ChatList 活挂载行已建（virtual_list _live 非空）")
	if live_ok:
		var script_paths := {}
		for idx in (live as Dictionary).keys():
			var row: Node = (live as Dictionary)[idx]
			if row != null and row.get_script() != null:
				script_paths[(row.get_script() as Script).resource_path] = true
		_assert(script_paths.has("res://scripts/rows/user_row.gd"), "user 行 = user_row.gd 实例")
		_assert(script_paths.has("res://scripts/rows/tool_row.gd"), "tool 行 = tool_row.gd 实例")
	# 后端契约：事件落库 -> 切走再切回时 session.history 必须重放本轮。
	_histories["s1"] = TURN_EVENTS.duplicate(true)
	return true


func tool_payload_get_name(payload: Dictionary) -> String:
	return str(payload.get("name", ""))


## —— 切会话 resume + history 回放 ————————————————————————————
func _verify_session_switch() -> void:
	print("== 切会话 resume + history ==")
	client_rpc_clear()
	app._on_session_picked("s2")
	await _frames(10)
	var calls := client_rpc_calls("session.history") + client_rpc_calls("session.resume")
	var ids := {}
	for entry in calls:
		var p: Dictionary = (entry as Dictionary).get("payload", {})
		ids[str(p.get("sessionId", ""))] = true
	_assert(ids.has("s2"), "切换触发 session.history + session.resume（sessionId=s2）")
	_assert(app._active_id() == "s2", "活动会话切到 s2")
	var kinds := chat_kinds()
	_assert(kinds == ["user", "assistant"], "历史回放 kinds=%s（s2 旧对话）" % str(kinds))
	if kinds.size() == 2:
		var nodes := chat_nodes()
		_assert(str((nodes[0]["payload"] as Dictionary).get("text", "")) == "s2 历史提问",
				"user 气泡在历史回放中重现")
		_assert(str((nodes[1]["payload"] as Dictionary).get("text", "")) == "s2 历史回答",
				"旧 assistant 文本完整回放")
	_assert(bool(app._composer.call("is_generating")) == false,
			"切会话后 composer 不带 generating 残留")
	# 再回 s1：上一轮的 live 流内容不串台（历史 epoch 幂等）
	client_rpc_clear()
	app._on_session_picked("s1")
	await _frames(10)
	var kinds_back := chat_kinds()
	_assert(kinds_back == ["user", "assistant", "tool"] and str((chat_nodes()[2]["payload"] as Dictionary).get("name", "")) == "fs.read",
			"切回 s1 重放完整本轮（user→assistant→tool，无丢行）")
	_assert(app._active_id() == "s1", "切回 s1 生效")