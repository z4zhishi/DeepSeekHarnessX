extends Node
## 审批卡自动化探针（headless 可跑）：
##   1) 实例化 ApprovalOverlay，模拟 host/permission-request 打开卡片；
##   2) 断言结构化摘要解析出工具名；
##   3) 模拟按键 1 -> 断言 allow_once 决策信号；
##   4) 再次打开，模拟 host/permission-resolved 帧 -> 断言卡片自动关闭；
##   5) 全部通过 quit(0)，任一失败打印 APPROVAL_FAIL 并 quit(1)。
## 运行：godot --headless --path . res://tests/probe_approval.tscn

var _overlay: CanvasLayer
var _failures: PackedStringArray = PackedStringArray()
var _last_decision := {"call_id": "", "decision": ""}
var _t0 := 0


func _stamp(m: String) -> void:
	var line := "%8.3fs %s" % [(Time.get_ticks_msec() - _t0) / 1000.0, m]
	print(line)
	var f := FileAccess.open("user://approval_probe_log.txt", FileAccess.READ_WRITE)
	if f == null:
		f = FileAccess.open("user://approval_probe_log.txt", FileAccess.WRITE)
	if f:
		f.seek_end()
		f.store_line(line)
		f.close()


func _check(cond: bool, what: String) -> bool:
	if cond:
		_stamp("PASS: " + what)
	else:
		_failures.append(what)
		_stamp("FAIL: " + what)
	return cond


func _ready() -> void:
	_t0 = Time.get_ticks_msec()
	var f := FileAccess.open("user://approval_probe_log.txt", FileAccess.WRITE)
	f.store_line("=== approval probe start ===")
	f.close()
	_run()


func _run() -> void:
	var scene: PackedScene = load("res://scenes/overlays/approval.tscn")
	_overlay = scene.instantiate()
	add_child(_overlay)
	await get_tree().process_frame

	if not _overlay.has_signal("decision_made"):
		_check(false, "overlay has decision_made signal")
		get_tree().quit(1)
		return
	_overlay.decision_made.connect(_on_decision)

	# --- 阶段 A：打开 + 结构化摘要 ---
	var tool_prompt := "Allow tool 'write_file' with args: {\"path\": \"a.txt\", \"content\": \"hello\"}?"
	_overlay.show_request("approval-test-1", tool_prompt, ["allow_once", "deny", "cancel"])
	await get_tree().process_frame
	_check(_overlay.visible, "card visible after show_request")
	_check(str(_overlay._call_id) == "approval-test-1", "call id captured")
	var summary_visible: bool = _overlay._summary_box.visible
	var tool_text: String = _overlay._tool_label.text
	_check(summary_visible and tool_text == "write_file",
		"structured summary parsed tool name (got '%s')" % tool_text)
	var btn_texts := _button_texts()
	_check(btn_texts.size() == 4, "4 buttons rendered incl. allow_all (got %d)" % btn_texts.size())
	var has_hint := false
	for t in btn_texts:
		if t.contains("[1]"):
			has_hint = true
	_check(has_hint, "primary button labeled with key hint [1]")

	# --- 阶段 B：按键 1 -> allow_once ---
	_press_key(KEY_1)
	await get_tree().process_frame
	await get_tree().process_frame
	_check(_last_decision["decision"] == "allow_once" and _last_decision["call_id"] == "approval-test-1",
		"key 1 emitted allow_once for approval-test-1")

	# 等待 fade_out 完成后卡片应完全不可见（CanvasLayer 无 modulate，只看 visible）
	for i in 60:
		await get_tree().process_frame
		if not _overlay.visible:
			break
	_check(not _overlay.visible, "card fully hidden after fade")

	# --- 阶段 C：重开卡片 + 模拟 resolved 帧自动关闭 ---
	_overlay.show_request("approval-test-2", tool_prompt, [])
	await get_tree().process_frame
	_check(_overlay.visible, "card reopened for resolved test")
	var resolved: bool = _overlay.resolve_remote("approval-test-2", "timeout")
	await get_tree().process_frame
	_check(resolved, "resolve_remote matched open call id")
	for i in 30:
		await get_tree().process_frame
		if not _overlay.visible:
			break
	_check(not _overlay.visible, "resolved frame auto-closed card")
	_check(_last_decision["decision"] != "cancel" or _last_decision["call_id"] != "approval-test-2",
		"remote resolve did not emit local decision signal")

	# --- 阶段 D：不匹配 callId 的 resolved 帧必须被忽略 ---
	_overlay.show_request("approval-test-3", tool_prompt, [])
	await get_tree().process_frame
	var ignored: bool = not _overlay.resolve_remote("other-call-id", "timeout")
	_check(ignored and _overlay.visible, "mismatched resolve_remote ignored, card stays open")
	# 收尾：Esc 关闭
	_press_key(KEY_ESCAPE)
	await get_tree().process_frame
	for i in 30:
		await get_tree().process_frame
		if not _overlay.visible:
			break

	if _failures.is_empty():
		_stamp("APPROVAL_DONE EXIT=0 all checks passed")
		get_tree().quit(0)
	else:
		_stamp("APPROVAL_FAIL EXIT=1 failures: %s" % ", ".join(_failures))
		get_tree().quit(1)


func _on_decision(call_id: String, decision: String) -> void:
	_last_decision = {"call_id": call_id, "decision": decision}
	_stamp("decision signal: %s -> %s" % [call_id, decision])


func _press_key(keycode: Key) -> void:
	var ev := InputEventKey.new()
	ev.keycode = keycode
	ev.physical_keycode = keycode
	ev.pressed = true
	Input.parse_input_event(ev)


func _button_texts() -> Array:
	var out: Array = []
	for b in _overlay._btn_row.get_children():
		if b is Button:
			out.append(str(b.text))
	return out
