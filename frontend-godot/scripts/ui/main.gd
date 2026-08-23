extends Control

@onready var client: DshClient = $DshClient
@onready var store: SessionStore = $SessionStore
@onready var session_list: ItemList = $HBoxContainer/Sidebar/SessionList
@onready var chat_view: ChatView = $HBoxContainer/MainArea/ChatScroll
@onready var input_dock: InputDock = $HBoxContainer/MainArea/InputDock
@onready var subagent_tree: SubagentTree = $HBoxContainer/Sidebar/SubagentTree
@onready var trajectory_canvas: TrajectoryCanvas = $HBoxContainer/MainArea/TrajectoryCanvas
@onready var approval_modal: ApprovalModal = $ApprovalModal
@onready var status_label: Label = $HBoxContainer/MainArea/HeaderBar/StatusLabel

var current_session_id: String = ""
var _switching: bool = false
var _streaming_seen: bool = false
var _reasoning_boxes: Array = []
var _terminal_cards: Dictionary = {}  # call_id -> TerminalBlock
var _subagent_session_ids: Array = []  # 本会话发起的子会话 id

func _ready() -> void:
	client.session_event_received.connect(_on_session_event)
	client.host_event_received.connect(_on_host_event)
	client.connection_state_changed.connect(_on_connection_state)
	
	input_dock.prompt_submitted.connect(_on_prompt_submitted)
	input_dock.file_reference_requested.connect(_on_file_reference)
	approval_modal.decision_made.connect(_on_approval_decision)
	subagent_tree.subagent_selected.connect(_switch_to_session)
	$HBoxContainer/Sidebar/NewSessionBtn.pressed.connect(_on_new_session_pressed)
	session_list.item_selected.connect(_on_session_selected)
	
	# 严格就绪握手：host.describe → 双路 WS → 基线同步
	client.describe(func(ok, _data):
		if ok:
			status_label.text = "Handshake OK (Ready)"
			status_label.modulate = Color(0.2, 0.9, 0.3)
		else:
			status_label.text = "Backend unreachable"
			status_label.modulate = Color(0.9, 0.3, 0.2)
	)
	client.list_sessions(func(ok, sessions):
		if ok and sessions is Array:
			for s in sessions:
				if s is Dictionary and s.has("id"):
					session_list.add_item("Session " + s["id"].substr(0, 8))
					session_list.set_item_metadata(session_list.item_count - 1, {"id": s["id"]})
	)
	_on_new_session_pressed()

func _on_connection_state(connected: bool) -> void:
	if connected:
		status_label.text = "Connected (Ready)"
		status_label.modulate = Color(0.2, 0.9, 0.3)
	else:
		status_label.text = "Disconnected"
		status_label.modulate = Color(0.9, 0.3, 0.2)

func _on_new_session_pressed() -> void:
	client.create_session(".", "default", func(ok, data):
		if ok and data.has("id"):
			_switch_to_session(data["id"], true)
	)

func _on_session_selected(index: int) -> void:
	var label: String = session_list.get_item_text(index)
	var sid := label.replace("Session ", "")
	if sid == "":
		return
	# 只保留已知 id 映射：ItemList 文本前缀会话 id 前 8 位，
	# 通过列表项元数据存全 id。
	var meta: Variant = session_list.get_item_metadata(index)
	if meta is Dictionary and meta.has("id"):
		_switch_to_session(str(meta["id"]), true)

func _switch_to_session(session_id: String, load_history: bool) -> void:
	if _switching:
		return
	_switching = true
	current_session_id = session_id
	store.set_active(session_id)
	chat_view.clear_messages()
	trajectory_canvas.clear_all()
	_reasoning_boxes.clear()
	_terminal_cards.clear()
	_streaming_seen = false
	client.set_active_session(session_id)
	var full_id := session_id
	# 更新侧边栏：避免重复项
	var found := -1
	for i in session_list.item_count:
		var meta: Variant = session_list.get_item_metadata(i)
		if meta is Dictionary and meta.get("id", "") == full_id:
			found = i
			break
	if found == -1:
		session_list.add_item("Session " + full_id.substr(0, 8))
		session_list.set_item_metadata(session_list.item_count - 1, {"id": full_id})
		session_list.select(session_list.item_count - 1)
	else:
		session_list.set_item_metadata(found, {"id": full_id})
		session_list.select(found)
	_switching = false
	_add_system_message("Started session: " + full_id)
	if load_history:
		client.fetch_history(full_id, 0, func(ok, events):
			if ok and events is Array:
				for env in events:
					_on_session_event(env)
		)

func _on_prompt_submitted(text: String) -> void:
	if text == "" or current_session_id == "":
		return
	if text.begins_with("/"):
		# Slash commands resolve through the shared backend registry
		# (command/run -> command/done lifecycle in the session log).
		client.send_command(current_session_id, text, func(ok, resp):
			if ok and resp is Dictionary:
				var text_out: String = resp.get("text", "")
				if text_out != "":
					chat_view.add_message("user", text)
					chat_view.add_message("assistant", text_out)
			else:
				_add_system_message("Unknown command: " + text)
		)
		return
	chat_view.add_message("user", text)
	client.send_prompt(current_session_id, text, func(result, _resp):
		if not result:
			_add_system_message("Failed to send prompt.")
	)

func _on_file_reference() -> void:
	_add_system_message("File reference: paste a workspace-relative path into the prompt.")

func _on_approval_decision(call_id: String, decision: String) -> void:
	client.respond_approval(call_id, decision, func(_ok, _resp):
		pass
	)

func _on_session_event(env: Dictionary) -> void:
	store.append_event(env)
	var type = env.get("type", "")
	var data = env.get("data", {})
	
	match type:
		"turn/start":
			_streaming_seen = false
			_add_system_message("=== Turn " + str(data.get("turn", 1)) + " Started ===")
			trajectory_canvas.record_turn_start(data.get("turn", 1))
		
		"assistant/chunk":
			var chunk = data.get("chunk", {})
			var ctype: String = chunk.get("type", "")
			if ctype == "text-delta":
				_streaming_seen = true
				chat_view.append_streaming(chunk.get("text", ""))
			elif ctype == "reasoning-delta":
				var box := _active_reasoning_box()
				box.append_delta(chunk.get("text", ""))
			elif ctype == "tool-call-delta":
				pass
		
		"assistant/message":
			var msg = data.get("message", {})
			var content = msg.get("content", [])
			var saw_text := false
			for block in content:
				match block.get("type", ""):
					"text":
						saw_text = true
						# 流式渲染过的文本不再重复建卡；仅 history 回放（无 chunk）时直加
						if not _streaming_seen:
							chat_view.add_message("assistant", block.get("text", ""))
					"reasoning":
						# 流式渲染过的推理仅补 meta；history 回放（无 delta）直接建卡
						if _reasoning_boxes.size() > 0:
							_reasoning_boxes[_reasoning_boxes.size() - 1].finish(0, 0)
						else:
							var rbox := _new_reasoning_box()
							rbox.append_delta(block.get("text", ""))
							rbox.finish(0, 0)
					"tool-call":
						pass
			chat_view.end_streaming()
			trajectory_canvas.record_event("assistant")
			
		"tool/call":
			var name = data.get("name", "")
			var args = data.get("arguments", "")
			chat_view.add_message("tool", name + " " + args)
			trajectory_canvas.record_event("tool: " + name)
			
		"tool/result":
			var msg = data.get("message", {})
			var is_err = false
			var text = ""
			var call_id: String = ""
			var content = msg.get("content", [])
			for block in content:
				if block.get("type", "") == "tool-result":
					is_err = block.get("isError", false)
					call_id = block.get("toolCallId", "")
					for b in block.get("content", []):
						if b.get("type", "") == "text":
							text += b.get("text", "")
			if _terminal_cards.has(call_id):
				var tblock: TerminalBlock = _terminal_cards[call_id]
				tblock.append_raw_ansi(text)
				tblock.finish()
				_terminal_cards.erase(call_id)
			elif _looks_like_diff(text):
				var dblock := DiffBlock.new()
				dblock.setup(text, "Unified Diff")
				chat_view.add_card(dblock)
			else:
				var prefix := "ERROR: " if is_err else ""
				chat_view.add_message("tool", prefix + text)
			trajectory_canvas.record_event("result")
			
		"turn/end":
			_add_system_message("=== Turn Completed ===")
			trajectory_canvas.record_turn_end()

func _new_reasoning_box() -> ReasoningBox:
	var box := ReasoningBox.new()
	box.begin()
	chat_view.add_card(box)
	return box

func _active_reasoning_box() -> ReasoningBox:
	if _reasoning_boxes.is_empty():
		var box := _new_reasoning_box()
		_reasoning_boxes.append(box)
	return _reasoning_boxes[_reasoning_boxes.size() - 1]

func _looks_like_diff(text: String) -> bool:
	var lines := text.split("\n")
	if lines.size() < 3:
		return false
	var hunks := 0
	for line in lines:
		if line.begins_with("diff --git") or line.begins_with("@@ "):
			hunks += 1
	return hunks >= 1

func _first_line(json_str: String) -> String:
	var json := JSON.new()
	if json.parse(json_str) == OK:
		var data = json.get_data() as Dictionary
		if data.has("command"):
			var cmd: String = str(data["command"])
			return cmd.split("\n")[0]
	return json_str.replace(" ", " ").substr(0, 60)

func _on_host_event(method: String, payload: Variant) -> void:
	match method:
		"host/session-added":
			var s = payload as Dictionary
			if s.has("id") and s["id"] != current_session_id:
				session_list.add_item("Session " + s["id"].substr(0, 8))
				session_list.set_item_metadata(session_list.item_count - 1, {"id": s["id"]})
		"host/permission-request":
			var req = payload as Dictionary
			var call_id: String = req.get("callId", "")
			var prompt: String = req.get("prompt", "Allow this tool call?")
			var opts: Array = req.get("options", []) as Array
			approval_modal.show_request(call_id, prompt, opts)
		"host/subagent-started":
			var started = payload as Dictionary
			subagent_tree.add_subagent(
				started.get("parentSessionId", ""),
				started.get("childSessionId", ""))
		"host/subagent-finished":
			var finished = payload as Dictionary
			subagent_tree.finish_subagent(
				finished.get("childSessionId", ""),
				finished.get("status", "done"),
				finished.get("stopReason", ""))

func _add_system_message(text: String) -> void:
	chat_view.add_message("system", text)

func _add_assistant_message(text: String) -> void:
	chat_view.add_message("assistant", text)