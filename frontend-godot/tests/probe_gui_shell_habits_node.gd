extends "res://tests/probe_gui_shell_base.gd"

## GUI 习惯链探针（Goal §3「GUI 习惯链」行 / W11 行为半边）。
##
## 五组断言（真实 app 树 + mock 网关，无平行路径）：
##   1. 审批/编辑贴输入左下：%AccessBtn 与 %RejectAllBtn 都在 %LeftChrome 下，
##      %ApprovalOverlay 常备（reject_all 关闭时才弹卡）
##   2. 模型·effort 一钮 & 弹层不关：%ModelEffortBtn 在 %RightChrome 且文案含
##      `·`；pressed 开弹层；弹层内换模型/effort 后弹层仍开；只有层外点击
##      语义（_hide_pops_if_outside）才关
##   3. 自动拒绝不阻断：reject_all 开 -> host/permission-request 直达
##      approval.respond deny，不弹卡、不抢焦点；对照关闭 -> 卡弹出
##   4. 多会话置顶 + 快捷键：pinned 顺序出现在 sidebar 与 Ctrl+K 切换器首部；
##      Ctrl+K 开关器、选中即切；Ctrl+Tab 沿 sidebar 顺序换会话
##   5. 全局命令面板：Ctrl+P 走真实 app._unhandled_input -> composer 公共入口
##      open_command_palette()：聚焦输入、注入 "/"、弹出命令候选；Esc 收起沿用
##      composer 既有语义（supremacy-plan §3 命令面板行）
##
## user:// 备份/恢复：pinned_sessions.json、approval_auto_reject.txt（基类负责）
## RESULT：GUI_SHELL_RESULT passed=N failed=M


func result_tag() -> String:
	return "GUI_SHELL_RESULT"


const MODELS := [
	{"id": "deepseek-chat", "name": "DeepSeek Chat"},
	{"id": "deepseek-reasoner", "name": "DeepSeek Reasoner"},
]


func _run() -> void:
	snapshot_user_files()
	write_pins(["s2", "s1"])
	var loaded := await boot_app([
		{"id": "s1", "title": "会话一", "cwd": "C:/repo"},
		{"id": "s2", "title": "会话二", "cwd": "C:/repo"},
		{"id": "s3", "title": "会话三", "cwd": "D:/ws"},
	], {}, MODELS)
	if not loaded:
		_assert(false, "app.tscn + mock 网关装载")
		return
	_verify_approval_chrome()
	await _verify_model_effort_popover()
	await _verify_auto_reject()
	await _verify_pinned_and_hotkeys()
	await _verify_global_command_palette()


# 1 ---------------------------------------------------------------------------

func _verify_approval_chrome() -> void:
	print("== 审批/编辑在输入左下 ==")
	var composer := app.get_node("%Composer") as Control
	var left := composer.get_node("%LeftChrome") as Control
	var access := composer.get_node("%AccessBtn") as Button
	var reject := composer.get_node("%RejectAllBtn") as Button
	if left == null or access == null or reject == null:
		_assert(false, "LeftChrome / AccessBtn / RejectAllBtn 可解析")
		return
	_assert(left.is_ancestor_of(access), "%AccessBtn 在 %LeftChrome 下（审批贴输入左下）")
	_assert(left.is_ancestor_of(reject), "%RejectAllBtn（需审批/自动拒绝）也在左下 chrome")
	var presets: Variant = composer.get("ACCESS_PRESETS")
	_assert(presets is PackedStringArray and (presets as PackedStringArray).has("default")
			and (presets as PackedStringArray).has("accept-edits"),
			"审批(default)/编辑(accept-edits) 档位合同仍在")
	_assert(app.get_node("%ApprovalOverlay") != null, "全屏审批 overlay 常驻存在")


# 2 ---------------------------------------------------------------------------

func _verify_model_effort_popover() -> void:
	print("== 模型·effort 一钮且弹层不关 ==")
	var composer := app.get_node("%Composer") as Control
	var btn := composer.get_node("%ModelEffortBtn") as Button
	var right := composer.get_node("%RightChrome") as Control
	if btn == null or right == null:
		_assert(false, "ModelEffortBtn / RightChrome 可解析")
		return
	_assert(right.is_ancestor_of(btn), "模型·effort 一钮位于 %RightChrome（贴输入右下）")
	_assert(btn.text.find("·") >= 0, "一钮文案是「模型 · 等级」复合（含 ·）")
	_assert(not btn.disabled, "mock 模型注入后一钮可用（非禁用）")
	# 开弹层（真实 pressed 信号路径）
	btn.pressed.emit()
	await _frames(2)
	var pop: PanelContainer = (composer.get("_model_pop") as PanelContainer)
	var pop_ctrl := pop
	_assert(pop_ctrl != null and pop_ctrl.visible, "ModelEffortBtn pressed 打开模型弹层")
	var model_list := (composer.get("_model_list") as ItemList)
	if model_list == null:
		_assert(false, "弹层模型列表 _model_list 存在")
		return
	_assert(model_list.item_count == MODELS.size(),
			"弹层列出全部模型（%d/%d）" % [model_list.item_count, MODELS.size()])
	# 弹层内选模型：ItemList 真信号 -> _on_pop_model
	var picked_box: Array = []
	composer.model_selected.connect(func(id: String) -> void: picked_box.append(id))
	model_list.item_selected.emit(1)
	await _frames(2)
	_assert(picked_box == ["deepseek-reasoner"], "弹层选模型经 model_selected(id)")
	_assert(client_rpc_calls("model.set").size() == 1, "app._on_model 转发 model.set RPC")
	_assert(pop_ctrl != null and pop_ctrl.visible, "换模型后弹层不关闭（弹层点选不关）")
	# 弹层内选 effort：effort 按钮真信号 -> app -> session.effort；弹层仍开
	var effort_box: Array = []
	composer.effort_changed.connect(func(e: String) -> void: effort_box.append(e))
	var low_picked := false
	var effort_btns: Variant = composer.get("_effort_btns")
	if effort_btns is Array:
		for raw in (effort_btns as Array):
			var b := raw as Button
			if b == null:
				continue
			if b.text.find("low") >= 0 or b.text.find("低") >= 0:
				b.pressed.emit()
				low_picked = true
				break
	_assert(low_picked, "弹层内找到低档 effort 按钮并点选")
	await _frames(2)
	_assert(effort_box == ["low"], "弹层选 effort 经 effort_changed")
	var effort_rpc := client_rpc_calls("session.effort")
	_assert(effort_rpc.size() == 1
			and str((effort_rpc[0] as Dictionary).get("payload", {}).get("effort", "")) == "low",
			"effort 选择走 session.effort RPC（app._on_param_effort）")
	_assert(str(composer.call("current_effort")) == "low", "composer effort 同步为 low")
	_assert(pop_ctrl != null and pop_ctrl.visible, "换 effort 后弹层仍开（同层不互斥）")
	var btn2 := composer.get_node("%ModelEffortBtn") as Button
	_assert(btn2_text_has_dot(btn), "一钮文案仍保持「模型 · 等级」复合形态")
	# 对照：层外点击语义才关闭（_hide_pops_if_outside 是唯一的非点选关闭路径）
	composer.call("_hide_pops_if_outside", Vector2(4, 4))
	await _frames(2)
	_assert(pop_ctrl != null and not pop_ctrl.visible, "层外点击才关闭弹层（对照）")


func btn2_text_has_dot(btn: Button) -> bool:
	return btn.text.find("·") >= 0


# 3 ---------------------------------------------------------------------------

func _verify_auto_reject() -> void:
	print("== 自动拒绝不阻断 ==")
	var composer := app.get_node("%Composer") as Control
	var reject := composer.get_node("%RejectAllBtn") as Button
	var overlay := app.get_node("%ApprovalOverlay")
	if reject == null or overlay == null or app._approvals == null:
		_assert(false, "RejectAllBtn / ApprovalOverlay / approval center 可达")
		return
	# 打开自动拒绝（真实 toggled 信号路径，持久化到 user:// 由基类备份恢复）
	reject.set_pressed(true)
	await _frames(2)
	_assert(bool(composer.call("is_reject_all")) == true, "reject_all 真实置位（is_reject_all）")
	_assert(bool(app.get("_approvals").get("auto_reject")) == true,
			"reject_all_toggled 同步进 approval center（auto_reject=true）")
	var before_focus := app.get_viewport().gui_get_focus_owner()
	# 下行一条真实权限请求帧（host WS -> app._on_host_event）
	client.call("deliver_host_event", "host/permission-request", {
		"callId": "call-auto-1",
		"sessionId": app._active_id(),
		"prompt": "Allow tool 'fs.write' with args: {\"path\":\"/tmp/x\"}?",
		"options": [{"optionId": "allow_once", "name": "允许一次"}],
	})
	await _frames(4)
	var responds := client_rpc_calls("approval.respond")
	_assert(responds.size() == 1
			and str((responds[0] as Dictionary).get("payload", {}).get("callId", "")) == "call-auto-1"
			and str((responds[0] as Dictionary).get("payload", {}).get("decision", "")) == "deny",
			"自动拒绝直发 approval.respond(callId, deny)，无人工介入")
	_assert(overlay.visible == false, "自动拒绝不弹全屏审批卡（overlay 保持隐藏）")
	var center: Variant = app.get("_approvals")
	var entry: Variant = center.call("get_item", "call-auto-1")
	_assert(entry != null and str(entry.get("state")) == "resolved"
			and str(entry.get("outcome")) == "denied",
			"center 记录该请求终态 resolved/denied（不滞留 pending）")
	_assert(center.call("pending_for_session", app._active_id()).size() == 0,
			"pending_for_session 为空（自动拒绝不留未决卡）")
	var after_focus := app.get_viewport().gui_get_focus_owner()
	_assert((before_focus == null and after_focus == null) or before_focus == after_focus,
			"自动拒绝不抢焦点（focus %s -> %s）" % [_node_label(before_focus), _node_label(after_focus)])
	var kinds := chat_kinds()
	_assert(kinds.size() > 0 and kinds[kinds.size() - 1] == "system",
			"自动拒绝落一条 system 文案进聊天流（不阻断）")
	if kinds.size() > 0 and kinds[kinds.size() - 1] == "system":
		var last_payload: Dictionary = chat_nodes()[kinds.size() - 1]["payload"]
		_assert(str(last_payload.get("text", "")).find("自动拒绝") >= 0
				or str(last_payload.get("text", "")).find("denied") >= 0,
				"system 文案语义为「已自动拒绝」")
	# 对照：关掉自动拒绝 -> 真弹卡（阻断语义分支存在且可达）
	reject.set_pressed(false)
	await _frames(2)
	client.call("deliver_host_event", "host/permission-request", {
		"callId": "call-manual-2",
		"sessionId": app._active_id(),
		"prompt": "Allow tool 'fs.write' with args: {\"path\":\"/tmp/y\"}?",
		"options": [],
	})
	await _frames(4)
	_assert(overlay.visible == true, "reject_all 关闭后同一请求弹出审批卡（对照）")
	var manual := app.get_node("%ApprovalOverlay")
	if manual.has_method("hide_request"):
		manual.call("hide_request")
	await _frames(2)
	_assert(overlay.visible == false, "对照卡片已收（hide_request）")


func _node_label(n: Node) -> String:
	return "<null>" if n == null else str(n.get_class()) + ":" + str(n.name)


# 4 ---------------------------------------------------------------------------

func _verify_pinned_and_hotkeys() -> void:
	print("== 多会话置顶 + Ctrl+K / Ctrl+Tab ==")
	var sidebar: Node = app._sidebar
	var pins: PackedStringArray = sidebar.call("pinned_ids")
	_assert([str(pins[0] if pins.size() > 0 else ""), str(pins[1] if pins.size() > 1 else "")] == ["s2", "s1"],
			"user://pinned_sessions.json 读回置顶序 [s2, s1]")
	# sidebar 列表：置顶段 header + pinned 两行先于普通分组
	var list := sidebar.get("_list") as ItemList
	var header_kinds: Array = []
	var pin_positions: Array = []
	for i in list.item_count:
		var meta: Variant = list.get_item_metadata(i)
		if meta is Dictionary:
			var kind := str((meta as Dictionary).get("kind", ""))
			if kind == "cwd_header":
				header_kinds.append(str((meta as Dictionary).get("cwd", "")))
			elif kind == "session":
				var pid := str((meta as Dictionary).get("id", ""))
				if pins.has(pid) and pin_positions.size() < pins.size():
					pin_positions.append(i)
	_assert(header_kinds.size() > 0 and header_kinds[0] == "__pinned__",
			"sidebar 首个分组 header 是置顶组（__pinned__）")
	_assert(pin_positions.size() == pins.size()
			and list.get_item_metadata(pin_positions[0]).get("id", "") == "s2"
			and list.get_item_metadata(pin_positions[1]).get("id", "") == "s1",
			"置顶行按 [s2, s1] 顺序渲染于列表顶部")
	# Ctrl+K：打开会话切换器（真实 app._unhandled_input 路径）
	app._unhandled_input(key_event(KEY_K, true))
	await _frames(4)
	var switcher: Node = app.get("_switcher")
	_assert(switcher != null and bool(switcher.get("visible")), "Ctrl+K 打开多会话切换器")
	if switcher != null and bool(switcher.get("visible")):
		var slist := switcher.get("_list") as ItemList
		_assert(slist != null and slist.item_count >= 2, "切换器列出会话")
		var first_meta: Variant = slist.get_item_metadata(0)
		var second_meta: Variant = slist.get_item_metadata(1)
		_assert(first_meta is Dictionary and str(first_meta.get("id", "")) == "s2"
				and second_meta is Dictionary and str(second_meta.get("id", "")) == "s1",
				"切换器置顶序 [s2, s1] 置于列表头部")
		_assert(str(slist.get_item_text(0)).begins_with("• "), "置顶行在切换器中带置顶标记")
		# 选中即切（真实关闭+emit 流程）
		switcher.call("_pick_id", "s2")
		await _frames(8)
		_assert(app._active_id() == "s2", "切换器选中 -> 会话切到 s2")
		_assert(app.get("_switcher").get("visible") == false, "选择后切换器关闭")
	# Ctrl+Tab：sidebar select_relative(+1) 走真实切会话
	client_rpc_clear()
	var expected := _expected_relative(sidebar, app._active_id(), 1)
	app._unhandled_input(key_event(KEY_TAB, true))
	await _frames(10)
	_assert(app._active_id() == expected,
			"Ctrl+Tab 切到侧栏下一个会话（active=%s, 期望=%s）" % [app._active_id(), expected])
	_assert(client_rpc_calls("session.resume").size() >= 1, "Ctrl+Tab 切换同样触发 resume 链路")
	# Ctrl+Shift+Tab 反向
	var expected_back := _expected_relative(sidebar, app._active_id(), -1)
	app._unhandled_input(key_event(KEY_TAB, true, true))
	await _frames(10)
	_assert(app._active_id() == expected_back, "Ctrl+Shift+Tab 反向切换（active=%s）" % app._active_id())


func _expected_relative(sidebar: Node, current: String, delta: int) -> String:
	# 与 sidebar.select_relative 同序的可选会话 id 序列
	var list := sidebar.get("_list") as ItemList
	var ids: Array = []
	if list == null:
		return ""
	for i in list.item_count:
		if not list.is_item_selectable(i):
			continue
		var meta: Variant = list.get_item_metadata(i)
		if meta is Dictionary and str(meta.get("kind", "")) == "session":
			ids.append(str(meta.get("id", "")))
	var cur := ids.find(current)
	if cur < 0:
		return ""
	var idx := posmod(cur + delta, ids.size())
	return str(ids[idx])


# 5 ---------------------------------------------------------------------------

## 全局命令面板 Ctrl+P（supremacy-plan §3 命令面板行）：与 Ctrl+K 探针同机制
## ——把按键事件喂进真实 app._unhandled_input，断言 composer 公共入口
## open_command_palette() 的行为面：聚焦输入、注入 "/"、面板可见、Esc 收起。
## 命令清单走真实 app 链路（mock command.list -> app._refresh_commands ->
## composer.set_commands），不绕过公共 API 也不触碰 composer 私有状态。
func _verify_global_command_palette() -> void:
	print("== 全局命令面板 Ctrl+P ==")
	var composer := app.get_node("%Composer") as Control
	if composer == null:
		_assert(false, "%Composer 可解析")
		return
	var palette := composer.get_node("%CmdPalette") as ItemList
	var prompt := composer.get_node("%Prompt") as TextEdit
	if palette == null or prompt == null:
		_assert(false, "%CmdPalette / %Prompt 可解析")
		return
	# 命令清单经 mock 网关同路径注入（app._ready 已用空清单跑过一次 _refresh_commands）
	client.call("script_response", "command.list", func(_p: Dictionary, cb: Callable) -> void:
		cb.call(true, {"commands": [
			{"name": "help", "description": "用法"},
			{"name": "permission", "description": "审批模式"},
		]}))
	app.call("_refresh_commands")
	await _frames(2)
	# 前置：干净草稿 + 面板初始收起（Ctrl+P 的效果才可归因）
	composer.call("set_draft", "")
	await _frames(2)
	_assert(not palette.visible, "前置：命令面板初始收起")
	# 焦点先移去别处（SendBtn），Ctrl+P 必须把它抢回输入框
	var elsewhere := composer.get_node_or_null("%SendBtn") as Control
	if elsewhere != null:
		elsewhere.grab_focus()
		await _frames(1)
	var before_focus := app.get_viewport().gui_get_focus_owner()
	app._unhandled_input(key_event(KEY_P, true))
	await _frames(2)
	_assert(prompt.has_focus(), "Ctrl+P 聚焦 composer 输入框（focus %s -> prompt）" % _node_label(before_focus))
	_assert(str(composer.call("get_draft")).begins_with("/"),
			"Ctrl+P 注入 '/' 命令前缀（draft='%s'）" % str(composer.call("get_draft")))
	_assert(palette.visible and palette.item_count >= 1,
			"Ctrl+P 打开命令面板（item_count=%d）" % palette.item_count)
	# Esc 收起：composer._input 的既有 Esc 语义，不新增关闭路径
	composer.call("_input", key_event(KEY_ESCAPE, false))
	await _frames(2)
	_assert(not palette.visible, "Esc 收起命令面板（沿用既有语义）")
	_assert(str(composer.call("get_draft")).begins_with("/"), "Esc 只收面板不动草稿")