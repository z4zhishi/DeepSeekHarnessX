extends Control
## 3 栏 AppFrame 骨架（波 1 统一事件路由 + 响应式）。
## 布局：侧栏 280px / 会话主区 748px 居中 | 详情 360px 可折叠。
## 视口 <1024px 自动折叠侧栏到 56px 窄轨（CENTER_MIN=640 于波 3 细化）。

## ---- 布局常量（对齐上游 React ui-layout AppFrame）----
const SIDEBAR_W: float = 280.0
const SIDEBAR_RAIL_W: float = 56.0
const CENTER_MIN: float = 640.0
const DETAILS_W: float = 360.0
const VIEWPORT_NARROW: float = 1024.0

@onready var client: DshClient = $DshClient
@onready var store: SessionStore = $SessionStore
@onready var frame: HBoxContainer = $Frame
@onready var sidebar: VBoxContainer = $Frame/Sidebar
@onready var sidebar_collapse_btn: Button = $Frame/Sidebar/SidebarHeader/CollapseBtn
@onready var sidebar_app_title: Label = $Frame/Sidebar/SidebarHeader/AppTitle
@onready var new_session_btn: Button = $Frame/Sidebar/NewSessionBtn
@onready var session_list: ItemList = $Frame/Sidebar/SessionList
@onready var subagent_tree: SubagentTree = $Frame/Sidebar/SubagentTree
@onready var center: VBoxContainer = $Frame/Center
@onready var header_bar: HBoxContainer = $Frame/Center/HeaderBar
@onready var center_title: Label = $Frame/Center/HeaderBar/Title
@onready var chat_tab_btn: Button = $Frame/Center/HeaderBar/TabBar/ChatTabBtn
@onready var trajectory_tab_btn: Button = $Frame/Center/HeaderBar/TabBar/TrajectoryTabBtn
@onready var jobs_btn: Button = $Frame/Center/HeaderBar/JobsBtn
@onready var settings_btn: Button = $Frame/Center/HeaderBar/SettingsBtn
@onready var status_label: Label = $Frame/Center/HeaderBar/StatusLabel
@onready var model_picker: OptionButton = $Frame/Center/HeaderBar/ModelPicker
@onready var jobs_pop: PopupPanel = $JobsPop
@onready var jobs_list: ItemList = $JobsPop/Margin/V/JobList
@onready var jobs_refresh_btn: Button = $JobsPop/Margin/V/Btns/RefreshBtn
@onready var jobs_kill_btn: Button = $JobsPop/Margin/V/Btns/KillBtn
@onready var jobs_output: RichTextLabel = $JobsPop/Margin/V/JobOutput
@onready var settings_panel: PopupPanel = $SettingsPanel
@onready var settings_version: Label = $SettingsPanel/Margin/V/VersionLabel
@onready var settings_backend: Label = $SettingsPanel/Margin/V/BackendLabel
@onready var details_tool_picker: OptionButton = $Frame/Details/DetailsBody/ToolPicker
@onready var details_in_label: RichTextLabel = $Frame/Details/DetailsBody/InOutScroll/InOut/InLabel
@onready var details_out_label: RichTextLabel = $Frame/Details/DetailsBody/InOutScroll/InOut/OutLabel
@onready var chat_tab: VBoxContainer = $Frame/Center/ChatTab
@onready var chat_view: ChatView = $Frame/Center/ChatTab/ChatScroll
@onready var input_dock: InputDock = $Frame/Center/ChatTab/InputDock
@onready var trajectory_tab: VBoxContainer = $Frame/Center/TrajectoryTab
@onready var trajectory_canvas: TrajectoryCanvas = $Frame/Center/TrajectoryTab/TrajectoryCanvas
@onready var details: VBoxContainer = $Frame/Details
@onready var details_collapse_btn: Button = $Frame/Details/DetailsHeader/DetailsCollapseBtn
@onready var details_body: VBoxContainer = $Frame/Details/DetailsBody
@onready var approval_modal: ApprovalModal = $ApprovalModal

var current_session_id: String = ""
var _switching: bool = false
var _streaming_seen: bool = false
var _reasoning_boxes: Array = []
var _terminal_cards: Dictionary = {}  # call_id -> TerminalBlock
var _subagent_session_ids: Array = []  # 本会话发起的子会话 id
var _model_current: String = ""       # request/header config.model 只读展示（llm.models 未产时降级）
var _theme_choice: int = 0            # B5 theme 选择（System=0/Dark=1/Light=2）
var _ws_dialog: FileDialog = null     # New Session workspace 目录选择器
var _last_workspace_dir: String = ""

## 详情栏（选中 tool 卡 IN/OUT）数据
var _tool_call_ins: Array = []        # {name, callId, args}
var _tool_call_outs: Dictionary = {}  # callId -> out_text

## 响应式折叠状态
var _sidebar_collapsed: bool = false
var _details_collapsed: bool = false

func _ready() -> void:
	client.session_event_received.connect(_on_session_event)
	client.host_event_received.connect(_on_host_event)
	client.connection_state_changed.connect(_on_connection_state)
	client.jobs_refreshed.connect(_on_jobs_refreshed)

	input_dock.prompt_submitted.connect(_on_prompt_submitted)
	input_dock.file_reference_requested.connect(_on_file_reference)
	# B1 message-feedback：chat_view 的 like/dislike + note 按钮接后端 feedback.* RPC
	chat_view.feedback_rating.connect(_on_feedback_rating)
	chat_view.feedback_note.connect(_on_feedback_note)
	approval_modal.decision_made.connect(_on_approval_decision)
	subagent_tree.subagent_selected.connect(_switch_to_session)
	new_session_btn.pressed.connect(_on_new_session_pressed)
	session_list.item_selected.connect(_on_session_selected)

	# 波3：store 驱动的会话列表 / 面包屑 / 谱系归置
	store.sessions_changed.connect(_rebuild_session_list)
	store.active_session_changed.connect(_on_store_active_changed)
	store.lineage_changed.connect(_update_breadcrumb)

	# HeaderBar：Jobs / Settings 按钮
	jobs_btn.pressed.connect(func():
		_refresh_jobs()
		jobs_pop.popup_centered(Vector2(480, 420))
	)
	jobs_refresh_btn.pressed.connect(_refresh_jobs)
	jobs_kill_btn.pressed.connect(_kill_selected_job)
	jobs_list.item_selected.connect(_on_jobs_item_selected)
	settings_btn.pressed.connect(func():
		client.settings_describe(func(ok, data):
			if ok:
				_populate_settings(data)
			else:
				# 降级：后端未产 settings RPC，退化为 host.describe 运行时信息
				client.describe(func(_ok2, d2): _populate_settings(d2))
			# B5：settings 面板补充项（agent preset / permission presets / theme）
			_append_settings_extras()
		)
		settings_panel.popup_centered(Vector2(480, 560))
	)
	# 详情栏：选中 tool 卡 IN/OUT
	details_tool_picker.item_selected.connect(_on_details_tool_selected)
	# B5 模型选择器：item_selected 广播当前选择
	model_picker.item_selected.connect(_on_model_selected)

	# tab 切换（Chat / Trajectory 互斥）
	chat_tab_btn.toggled.connect(func(on: bool):
		_show_tab("chat") if on else _show_tab("trajectory")
	)
	trajectory_tab_btn.toggled.connect(func(on: bool):
		_show_tab("trajectory") if on else _show_tab("chat")
	)

	# 3 栏折叠按钮
	sidebar_collapse_btn.pressed.connect(func():
		_set_sidebar_collapsed(not _sidebar_collapsed)
	)
	details_collapse_btn.pressed.connect(func():
		_set_details_collapsed(not _details_collapsed)
	)

	resized.connect(_on_resized)
	call_deferred("_apply_frame_responsive")

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
					store.upsert_session(s)
	)
	_sync_model_picker()
	_on_new_session_pressed()

## ---- 3 栏响应式 ----

func _on_resized() -> void:
	call_deferred("_apply_frame_columns")

## 解析 3 栏宽度：窄视口自动折叠侧栏，详情栏可折叠到 0。
## center 用 size_flags_horizontal=3 吸收剩余宽度（= CENTER_MIN 语义，会话主区至少 640）。
func _apply_frame_responsive() -> void:
	var w: float = size.x
	if w < VIEWPORT_NARROW and not _sidebar_collapsed:
		_set_sidebar_collapsed(true)
	_apply_frame_columns()

func _apply_frame_columns() -> void:
	if _sidebar_collapsed:
		sidebar.custom_minimum_size.x = SIDEBAR_RAIL_W
		sidebar_app_title.visible = false
		sidebar_collapse_btn.text = "»"
	else:
		sidebar.custom_minimum_size.x = SIDEBAR_W
		sidebar_app_title.visible = true
		sidebar_collapse_btn.text = "«"
	if _details_collapsed:
		details.custom_minimum_size.x = 0.0
		details.visible = false
	else:
		details.custom_minimum_size.x = DETAILS_W
		details.visible = true
	# 会话主区：frame 撑满宽度时 size_flags_horizontal=3 自动吸收剩余，
	# 侧栏折叠腾出的空间让会话更宽，维持 CENTER_MIN 语义。
	center.custom_minimum_size.x = CENTER_MIN

func _set_sidebar_collapsed(collapsed: bool) -> void:
	_sidebar_collapsed = collapsed
	_apply_frame_columns()

func _set_details_collapsed(collapsed: bool) -> void:
	_details_collapsed = collapsed
	_apply_frame_columns()

func _abs_tab(tab: String) -> void:
	chat_tab.visible = (tab == "chat")
	trajectory_tab.visible = (tab == "trajectory")
	chat_tab_btn.button_pressed = (tab == "chat")
	trajectory_tab_btn.button_pressed = (tab == "trajectory")

func _show_tab(tab: String) -> void:
	_abs_tab(tab)

# ---- 状态 ----

func _on_connection_state(connected: bool) -> void:
	if connected:
		status_label.text = "Connected (Ready)"
		status_label.modulate = Color(0.2, 0.9, 0.3)
	else:
		status_label.text = "Disconnected"
		status_label.modulate = Color(0.9, 0.3, 0.2)

func _on_new_session_pressed() -> void:
	# New Session：先选 workspace 目录（目录模式 FileDialog），确认后建会话。
	_ensure_workspace_dialog()
	_ws_dialog.popup_centered_ratio(0.7)

## 目录选择 FileDialog（Godot FileDialog 目录模式）。
func _ensure_workspace_dialog() -> void:
	if _ws_dialog != null:
		return
	_ws_dialog = FileDialog.new()
	_ws_dialog.file_mode = FileDialog.FILE_MODE_OPEN_DIR
	_ws_dialog.access = FileDialog.ACCESS_FILESYSTEM
	_ws_dialog.title = "Select workspace directory"
	_ws_dialog.ok_button_text = "Start session"
	_ws_dialog.size = Vector2(600, 440)
	_ws_dialog.current_dir = _last_workspace_dir if _last_workspace_dir != "" else ProjectSettings.globalize_path("res://")
	_ws_dialog.dir_selected.connect(_on_workspace_dir_selected)
	add_child(_ws_dialog)

func _on_workspace_dir_selected(dir: String) -> void:
	if dir == "":
		return
	_last_workspace_dir = dir
	# 后端 cwd 用绝对路径；Windows 下统一斜杠
	var cwd := dir.replace("\\", "/")
	client.create_session(cwd, "default", func(ok, data):
		if ok and data.has("id"):
			_switch_to_session(data["id"], true)
	)

## 模型选择器：优先用后端 llm.models RPC 填充（可编辑）；失败时降级由
## request/header config.model 只读展示。选择变更经 model_picker.item_selected
## 广播（未接后端切换 RPC，先做前端本地选择 + 状态提示）。
func _sync_model_picker() -> void:
	if not is_instance_valid(model_picker):
		return
	model_picker.clear()
	model_picker.disabled = true
	client.list_models(func(ok, data):
		if ok and data is Dictionary and data.get("models") is Array:
			var models: Array = data["models"]
			var active: String = str(data.get("active", _model_current))
			var first_id := ""
			for m in models:
				if not (m is Dictionary):
					continue
				var id: String = str(m.get("id", ""))
				if id == "":
					continue
				var label := id
				var name: String = str(m.get("name", ""))
				if name != "":
					label = name
				model_picker.add_item(label)
				model_picker.set_item_metadata(model_picker.item_count - 1, {"id": id})
				if first_id == "":
					first_id = id
				if id == active or id == _model_current:
					model_picker.select(model_picker.item_count - 1)
			model_picker.disabled = model_picker.item_count == 0
		else:
			# 降级：仅展示当前模型（只读）
			if _model_current != "":
				model_picker.add_item(_model_current)
				model_picker.select(0)
				model_picker.disabled = true
	)

# B5：模型选择广播（本地记录，供 settings/status 展示当前选择）。
func _on_model_selected(index: int) -> void:
	if index < 0:
		return
	var meta: Variant = model_picker.get_item_metadata(index)
	if meta is Dictionary:
		_model_current = str(meta.get("id", ""))
		status_label.text = "Model: " + _model_current

## 由 store 全量重建侧栏会话列表（store 为唯一数据源）。
func _rebuild_session_list(_sessions: Array) -> void:
	var prev := current_session_id
	session_list.clear()
	for s in store.sessions:
		var id: String = str(s.get("id", ""))
		if id == "":
			continue
		var label := _session_label(s)
		session_list.add_item(label)
		session_list.set_item_metadata(session_list.item_count - 1, {"id": id})
		if id == prev:
			session_list.select(session_list.item_count - 1)

func _session_label(s: Dictionary) -> String:
	var title: String = str(s.get("title", ""))
	if title == "":
		title = "Session " + str(s.get("id", "")).substr(0, 8)
	var status: String = str(s.get("status", ""))
	if status != "":
		title += " [" + status + "]"
	return title

func _on_session_selected(index: int) -> void:
	var meta: Variant = session_list.get_item_metadata(index)
	if meta is Dictionary and meta.has("id"):
		_switch_to_session(str(meta["id"]), true)

## store 归置当前会话（active_session_id / 祖先链）变化。
func _on_store_active_changed(session_id: String) -> void:
	_ensure_in_list(session_id)
	center_title.text = " Active: " + _short_id(session_id)

func _ensure_in_list(session_id: String) -> void:
	if session_id == "":
		return
	var found := -1
	for i in session_list.item_count:
		var meta: Variant = session_list.get_item_metadata(i)
		if meta is Dictionary and str(meta.get("id", "")) == session_id:
			found = i
			break
	if found == -1:
		session_list.add_item(_short_id(session_id))
		session_list.set_item_metadata(session_list.item_count - 1, {"id": session_id})
		found = session_list.item_count - 1
	session_list.select(found)

## HeaderBar 会话头面包屑（store 祖先链，逐级可导航回根）。
func _update_breadcrumb() -> void:
	var chain: Array[String] = store.lineage()
	var parts: Array[String] = []
	for sid in chain:
		var s := store.get_session(sid)
		var name: String = str(s.get("title", "")) if not s.is_empty() else _short_id(sid)
		if name == "":
			name = _short_id(sid)
		parts.append(name)
	var text := " > ".join(parts) if parts.size() > 0 else "—"
	center_title.text = " " + text

func _short_id(session_id: String) -> String:
	return session_id.substr(0, 8) if session_id.length() > 8 else session_id

func _switch_to_session(session_id: String, load_history: bool) -> void:
	if _switching:
		return
	_switching = true
	current_session_id = session_id
	store.set_active(session_id)
	# 确保谱系根入树（父缺失自愈建档由 store/host 归置）
	subagent_tree.add_root_session(session_id)
	chat_view.clear_messages()
	trajectory_canvas.clear_all()
	_reasoning_boxes.clear()
	_terminal_cards.clear()
	_tool_call_ins.clear()
	_tool_call_outs.clear()
	details_tool_picker.clear()
	_details_show_tool()
	_streaming_seen = false
	client.set_active_session(session_id)
	_ensure_in_list(session_id)
	_switching = false
	_add_system_message("Started session: " + session_id)
	_refresh_context_pressure()
	if load_history:
		client.fetch_history(session_id, 0, func(ok, events):
			if ok and events is Array:
				for env in events:
					_on_session_event(env)
		)

# ---- 输入 ----

func _on_prompt_submitted(text: String) -> void:
	if text == "" or current_session_id == "":
		return
	if text.begins_with("/"):
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

## B1 message-feedback：like/dislike 按钮接 feedback.put（rating=like|dislike）。
func _on_feedback_rating(message_id: String, rating: String) -> void:
	if current_session_id == "" or message_id == "":
		return
	# 先读当前 item 的 version，再以乐观 CAS 写入（冲突时后端返回权威 item，忽略重试）
	client.feedback_list(current_session_id, func(ok, data):
		var version := ""
		if ok and data is Dictionary and data.get("items") is Array:
			for it in data["items"] as Array:
				if it is Dictionary and str(it.get("messageId", "")) == message_id:
					version = str(it.get("version", ""))
					break
		client.feedback_put(current_session_id, message_id, rating, "", version, func(_ok2, _resp):
			pass
		)
	)

## B1 message-feedback：note 弹层确认后写 feedback.put 的 note 字段（保留原 rating）。
func _on_feedback_note(message_id: String, note: String) -> void:
	if current_session_id == "" or message_id == "" or note == "":
		return
	client.feedback_list(current_session_id, func(ok, data):
		var version := ""
		var rating := "like"
		if ok and data is Dictionary and data.get("items") is Array:
			for it in data["items"] as Array:
				if it is Dictionary and str(it.get("messageId", "")) == message_id:
					version = str(it.get("version", ""))
					if it.get("rating", "") != "":
						rating = str(it.get("rating", ""))
					break
		client.feedback_put(current_session_id, message_id, rating, note, version, func(_ok2, _resp):
			pass
		)
	)

func _on_approval_decision(call_id: String, decision: String) -> void:
	client.respond_approval(call_id, decision, func(_ok, _resp):
		pass
	)

func _on_jobs_refreshed(jobs: Array) -> void:
	# 渲染 Jobs popover 列表
	jobs_list.clear()
	for j in jobs:
		if j is Dictionary:
			var id: String = str(j.get("id", ""))
			var label: String = str(j.get("kind", "")) + " · " + str(j.get("status", ""))
			if j.get("label", "") != "":
				label = str(j.get("label", "")) + " — " + label
			jobs_list.add_item(label)
			jobs_list.set_item_metadata(jobs_list.item_count - 1, {"id": id})

func _refresh_jobs() -> void:
	client.list_jobs(current_session_id, func(_ok, _data):
		pass  # jobs_refreshed 已由 client 转发
	)

func _kill_selected_job() -> void:
	var sel := jobs_list.get_selected_items()
	if sel.is_empty():
		return
	var idx: int = sel[0]
	var meta: Variant = jobs_list.get_item_metadata(idx)
	if meta is Dictionary and meta.has("id"):
		client.kill_job(current_session_id, str(meta["id"]), func(_ok, _data):
			_refresh_jobs()
		)

func _on_jobs_item_selected(index: int) -> void:
	var meta: Variant = jobs_list.get_item_metadata(index)
	if meta is Dictionary and meta.has("id"):
		_job_output_selected(str(meta["id"]))

func _job_output_selected(job_id: String) -> void:
	client.read_job_output(current_session_id, job_id, func(ok, data):
		if ok and data is Dictionary:
			jobs_output.text = str(data.get("output", ""))
		else:
			jobs_output.text = "(no output)"
	)

## ---- 详情栏：选中 tool 卡 IN/OUT ----

func _on_details_tool_selected(index: int) -> void:
	if index < 0 or index >= _tool_call_ins.size():
		_details_show_tool()
		return
	var entry: Dictionary = _tool_call_ins[index]
	details_in_label.text = _details_in_text(entry)
	var call_id: String = str(entry.get("callId", ""))
	details_out_label.text = "OUT:\n" + str(_tool_call_outs.get(call_id, "—"))

func _details_show_tool() -> void:
	details_in_label.text = "IN:  —"
	details_out_label.text = "OUT:  —"

func _details_in_text(entry: Dictionary) -> String:
	var name: String = str(entry.get("name", ""))
	var args: String = str(entry.get("args", ""))
	return "IN:  " + name + "\n" + _pretty_args(args)

func _pretty_args(args: String) -> String:
	if args == "":
		return ""
	var json := JSON.new()
	if json.parse(args) == OK:
		return _esc_bb(JSON.stringify(json.get_data(), "\t", false))
	return _esc_bb(args)

func _esc_bb(s: String) -> String:
	return s.replace("[", "[lb]").replace("]", "[/lb]")

# ---- 统一事件路由 ----

## 入口：把后端 session/event 分派到 会话(chat)/推理(reasoning)/工具(tool)/
## 轨迹(trajectory)/谱系(subagent) 各视图，并消费已落库的 view 与 reasoningTokens。
func _on_session_event(env: Dictionary) -> void:
	store.append_event(env)
	var type = env.get("type", "")
	var data = env.get("data", {})
	var seq: int = int(env.get("seq", 0))
	var t: int = int(env.get("time", 0))

	match type:
		"turn/start":
			_streaming_seen = false
			_add_system_message("=== Turn " + str(data.get("turn", 1)) + " Started ===")
			trajectory_canvas.record_turn_start(data.get("turn", 1))

		"assistant/chunk":
			_route_assistant_chunk(data)

		"assistant/message":
			_route_assistant_message(data, t)

		"tool/call":
			_route_tool_call(data)

		"tool/result":
			_route_tool_result(data)

		"reasoning/start":
			# 推理会话头部事件：建可折叠 thinking 卡（含 token 语义由 usage 消费）
			var box := _active_reasoning_box()
			box.append_delta("")

		"reasoning/delta":
			var box := _active_reasoning_box()
			box.append_delta(data.get("delta", ""))

		"turn/end":
			_add_system_message("=== Turn Completed ===")
			trajectory_canvas.record_turn_end()

		"goal/change":
			# 已建未接线的 GoalCard：whole-value 快照（create/edit/pause/resume/complete/block/clear）
			var goal_card := GoalCard.new()
			goal_card.setup(data)
			chat_view.add_card(goal_card)
			trajectory_canvas.record_event("goal: " + str(data.get("operation", "")))

		"todo/write":
			# 已建未接线的 TodoCard：whole-list 快照
			var todo_card := TodoCard.new()
			todo_card.setup(data)
			chat_view.add_card(todo_card)
			trajectory_canvas.record_event("todo")

		"schedule/change":
			# 已建未接线的 ScheduleCard：create/delete/dispatch 快照
			var sched_card := ScheduleCard.new()
			sched_card.setup(data)
			chat_view.add_card(sched_card)
			trajectory_canvas.record_event("schedule: " + str(data.get("operation", "")))

		"plan/mode":
			# 新 PlanCard：active/pending + /plan off 提示
			var plan_card := PlanCard.new()
			plan_card.setup(data)
			chat_view.add_card(plan_card)
			trajectory_canvas.record_event("plan" + ("/on" if bool(data.get("active", false)) else "/off"))

		"feedback/record":
			# 新 FeedbackCard：log-only 人声反馈
			var fb_card := FeedbackCard.new()
			fb_card.setup(data)
			chat_view.add_card(fb_card)
			trajectory_canvas.record_event("feedback")

		"team/member", "team/task", "team/message/queued", "team/message/delivered":
			# 新 TeamCard：member/task/message 生命周期（宽容渲染）
			var team_card := TeamCard.new()
			team_card.setup(type, data)
			chat_view.add_card(team_card)
			trajectory_canvas.record_event("team/" + type.split("/")[1])

		"tool-workflow/agent-start", "tool-workflow/agent-end":
			# 新 WorkflowCard：workflow 运行时生命周期（agent-start/agent-end）
			var wf_card := WorkflowCard.new()
			wf_card.setup(type, data)
			chat_view.add_card(wf_card)
			trajectory_canvas.record_event("workflow/agent")

		"step/start":
			trajectory_canvas.record_event("step/start")

		"step/end":
			trajectory_canvas.record_event("step/end")

		"request/header":
			# 请求头部快照：消费 config.model 更新模型选择器（只读降级），并记轨迹
			var header: Dictionary = data.get("header", {})
			var config: Dictionary = header.get("config", {})
			var model: String = str(config.get("model", ""))
			if model != "":
				_model_current = model
				_sync_model_picker()
			trajectory_canvas.record_event("request/header")

		_:
			# 未知事件：仍让谱系/轨迹占位消费，避免阻塞后续 agent 扩展
			_route_unknown(type, data)

## 会话视图：流式文本（assistant/chunk text-delta）。
func _handle_assistant_chunk(data: Dictionary) -> void:
	var chunk = data.get("chunk", {})
	var ctype: String = chunk.get("type", "")
	if ctype == "text-delta":
		_streaming_seen = true
		chat_view.append_streaming(chunk.get("text", ""))
	elif ctype == "reasoning-delta":
		var box := _active_reasoning_box()
		box.append_delta(chunk.get("text", ""))

## 推理视图：reasoning-delta 入折叠 thinking 卡。
func _handle_reasoning_delta(text: String) -> void:
	var box := _active_reasoning_box()
	box.append_delta(text)

func _route_assistant_chunk(data: Dictionary) -> void:
	var chunk = data.get("chunk", {})
	var ctype: String = chunk.get("type", "")
	match ctype:
		"text-delta":
			_handle_assistant_chunk(data)
		"reasoning-delta":
			_handle_reasoning_delta(chunk.get("text", ""))
		"tool-call-delta":
			# 流式工具调用：最终 name/args 由 tool/call 上可折叠 ToolRow，
			# delta 只是中间态（无 call_id），无需在此渲染，避免重复。
			pass

func _route_assistant_message(data: Dictionary, t: int) -> void:
	var msg = data.get("message", {})
	var content = msg.get("content", [])
	# B1：extract assistant message id（backend AssistantMessagePayload.message.id）
	var message_id: String = str(msg.get("id", ""))
	var saw_text := false
	for block in content:
		match block.get("type", ""):
			"text":
				saw_text = true
				if not _streaming_seen:
					chat_view.add_message("assistant", block.get("text", ""), message_id)
				elif message_id != "":
					# 流式路径已 append 的消息补记 id，供反馈按钮使用
					chat_view.set_last_assistant_message_id(message_id)
			"reasoning":
				# usage.reasoningTokens（data.usage）落地后由波2 写 meta；history 回放无 delta 直建卡
				var tokens := _usage_reasoning_tokens(data)
				if _reasoning_boxes.size() > 0:
					_reasoning_boxes[_reasoning_boxes.size() - 1].finish(0, tokens)
				else:
					var rbox := _new_reasoning_box()
					rbox.append_delta(block.get("text", ""))
					rbox.finish(0, tokens)
			"tool-call":
				pass
	chat_view.end_streaming()
	trajectory_canvas.record_event("assistant")

func _route_tool_call(data: Dictionary) -> void:
	var name = data.get("name", "")
	var args = data.get("arguments", "")
	var call_id: String = data.get("callId", "")
	var view: Variant = data.get("view", {})

	# 运行中 tool 卡：若后端已推 callView（tool/call 的 view.kind），先建 live 卡
	_draw_call_view(call_id, name, view)
	# 可折叠工具行：最终调用摘要（name + args 首行）上墙
	var row := ToolRow.new()
	row.setup(name, args)
	chat_view.add_card(row)
	trajectory_canvas.record_event("tool: " + name)

	# 详情栏 IN：登记选中 tool 卡
	_tool_call_ins.append({"name": name, "args": args, "callId": call_id})
	details_tool_picker.add_item(_short_id(call_id) + " · " + name)
	details_tool_picker.select(details_tool_picker.item_count - 1)
	_details_show_tool()

func _route_tool_result(data: Dictionary) -> void:
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
	# 优先消费后端已落库的 view（kind/diffs/terminal）——字段名对齐后端，
	# 非 React 的 card/oldText/newText。
	var view: Dictionary = data.get("view", {})
	var out_text := ""
	if not view.is_empty():
		_apply_view(call_id, view, is_err)
		out_text = _view_text(view)
	elif _terminal_cards.has(call_id):
		var tblock: TerminalBlock = _terminal_cards[call_id]
		tblock.append_raw_ansi(text)
		tblock.finish()
		_terminal_cards.erase(call_id)
		out_text = text
	elif _looks_like_diff(text):
		var dblock := DiffBlock.new()
		dblock.setup(text, "Unified Diff")
		chat_view.add_card(dblock)
		out_text = text
	else:
		var prefix := "ERROR: " if is_err else ""
		chat_view.add_message("tool", prefix + text)
		out_text = prefix + text
	trajectory_canvas.record_event("result")

	# B4 deliverables：tool/result 若携带产出文件，渲染产物 chips（点击打开）。
	_render_deliverables(data)

	# 详情栏 OUT：登记该 tool call 的输出
	if call_id != "":
		_tool_call_outs[call_id] = out_text
		_refresh_details_out(call_id)

## B4 产物 chips：从 tool/result 的 data/view 读取 producedFiles/files 数组，
## 每个条目 {path, name?} 渲染一个 DeliverableChip，点击 OS.shell_open 打开。
func _render_deliverables(data: Dictionary) -> void:
	var files: Array = []
	var src: Variant = data.get("producedFiles", data.get("files", []))
	if src is Array:
		files = src
	if files.is_empty() and data.get("view", {}) is Dictionary:
		var v: Dictionary = data["view"]
		var vf: Variant = v.get("producedFiles", v.get("files", []))
		if vf is Array:
			files = vf
	if files.is_empty():
		return
	for entry in files:
		if entry is Dictionary:
			var path: String = str(entry.get("path", entry.get("name", "")))
			if path == "":
				continue
			var name: String = str(entry.get("name", path.get_file()))
			var chip := DeliverableChip.new()
			chip.setup(name, path)
			chat_view.add_card(chip)
		elif entry is String:
			var p: String = str(entry)
			if p == "":
				continue
			var chip := DeliverableChip.new()
			chip.setup(p.get_file(), p)
			chat_view.add_card(chip)
	trajectory_canvas.record_event("deliverables")

func _route_unknown(type: String, data: Dictionary) -> void:
	# 预留：未知事件归谱系/轨迹归置，不吞掉（波2/波3 可在此扩展）
	trajectory_canvas.record_event(type)

## 从 view 提取纯文本，供详情栏 OUT 显示。
func _view_text(view: Dictionary) -> String:
	var kind: String = view.get("kind", "")
	match kind:
		"terminal":
			var term = view.get("terminal", {})
			var lines = term.get("lines", [])
			if lines is Array:
				return "\n".join(PackedStringArray(lines))
			return ""
		"diff":
			var diffs = view.get("diffs", [])
			var parts: PackedStringArray = PackedStringArray()
			if diffs is Array:
				for d in diffs:
					if d is Dictionary:
						parts.append(str(d.get("path", "")))
						parts.append("--- a/" + str(d.get("path", "")))
						parts.append("+++ b/" + str(d.get("path", "")))
			return "\n".join(parts)
		_:
			return str(view.get("text", view.get("raw", "")))

## 当前详情栏选中的 tool call 有输出时刷新 OUT。
func _refresh_details_out(call_id: String) -> void:
	if call_id == "":
		return
	var sel := details_tool_picker.selected
	if sel < 0 or sel >= _tool_call_ins.size():
		return
	if str(_tool_call_ins[sel].get("callId", "")) == call_id:
		details_out_label.text = "OUT:\n" + str(_tool_call_outs.get(call_id, "—"))

## 消费后端落库的 ToolResultView：kind = diff | terminal | text。
## diffs: [{path, old, new}]，terminal: {lines, exitCode}。
func _apply_view(call_id: String, view: Dictionary, is_err: bool) -> void:
	var kind: String = view.get("kind", "")
	match kind:
		"terminal":
			var term = view.get("terminal", {})
			var tblock := TerminalBlock.new()
			tblock.begin("Terminal")
			var lines = term.get("lines", [])
			if lines is Array:
				for ln in lines:
					tblock.append_raw_ansi(str(ln) + "\n")
			tblock.finish()
			chat_view.add_card(tblock)
		"diff":
			var diffs = view.get("diffs", [])
			if diffs is Array and diffs.size() > 0:
				for d in diffs:
					if d is Dictionary:
						var path: String = d.get("path", "")
						var old: String = d.get("old", "")
						var new: String = d.get("new", "")
						var dblock := DiffBlock.new()
						dblock.setup(_reconciled_diff(path, old, new), path)
						chat_view.add_card(dblock)
		_:
			# kind == "text" 或未知：落回纯文本（含 is_err 前缀）
			var t = view.get("text", "")
			if t == "":
				t = view.get("raw", "")
			if t == "":
				return
			var prefix := "ERROR: " if is_err else ""
			chat_view.add_message("tool", prefix + str(t))

## tool/call 的 callView：运行中先建 terminal/diff live 卡，挂到 _terminal_cards。
func _draw_call_view(call_id: String, name: String, view: Variant) -> void:
	if not (view is Dictionary):
		return
	var kind: String = (view as Dictionary).get("kind", "")
	if kind == "terminal":
		var tblock := TerminalBlock.new()
		tblock.begin(name)
		chat_view.add_card(tblock)
		_terminal_cards[call_id] = tblock

## 由 diff hunk 的 old/new 重建统一 diff 文本（真色卡由波2 diff_block 消费）。
func _reconciled_diff(path: String, old: String, new: String) -> String:
	var out := "diff --git a/" + path + " b/" + path + "\n"
	out += "--- a/" + path + "\n"
	out += "+++ b/" + path + "\n"
	out += "@@ -1," + str(old.split("\n").size()) + " +1," + str(new.split("\n").size()) + " @@\n"
	for l in old.split("\n"):
		out += "-" + l + "\n"
	for l in new.split("\n"):
		out += "+" + l + "\n"
	return out

func _usage_reasoning_tokens(data: Dictionary) -> int:
	var usage = data.get("usage", {})
	if usage is Dictionary:
		return int(usage.get("reasoningTokens", 0))
	return 0

## ---- 卡片/推理辅助 ----

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

# ---- host 事件（谱系 / 审批 / jobs）----

func _on_host_event(method: String, payload: Variant) -> void:
	match method:
		"host/session-added":
			var s = payload as Dictionary
			if s.has("id"):
				# 归置会话 + 谱系：父会话/祖先链进 store，侧栏/面包屑随之刷新
				if s.has("parentSessionId"):
					store.set_parent(str(s["id"]), str(s["parentSessionId"]))
				store.upsert_session(s)
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

## ---- settings 面板 ----
## 消费 A2 settings.describe 契约：{namespaces:[{ns,base,user,revision,schema,writable}], writable, hasDocument}。
## 显示可读 namespace 文档（schema 若为字符串/字典则 JSON 展示），字段编辑经 settings.mutate。

var _settings_ns: String = ""
var _settings_ops: Array = []   # 待提交的 {op:"set"|"unset", path, value?}

func _populate_settings(data: Variant) -> void:
	if not (data is Dictionary):
		settings_version.text = "Version: —"
		settings_backend.text = "Backend: —"
		return
	settings_version.text = "Version: " + str(data.get("version", data.get("name", "—")))
	settings_backend.text = "Backend: " + str(data.get("backend", data.get("protocol", "—")))

	# 命名空间文档区（复用 popup 已有 VBox：Header/VersionLabel/BackendLabel/SkeletonBody）
	var vbox := settings_panel.get_node("Margin/V") as VBoxContainer
	# 清空旧的文档块（除固定头部三个节点外）
	for c in vbox.get_children():
		if c.name != "Header" and c.name != "VersionLabel" and c.name != "BackendLabel":
			vbox.remove_child(c)
			c.queue_free()

	var namespaces: Array = data.get("namespaces", []) as Array
	_settings_ops.clear()
	_settings_ns = ""

	if namespaces.size() == 0:
		# 后端未产 settings 契约：展示宿主能力 + 当前模型（只读降级）
		var hint := Label.new()
		hint.autowrap_mode = TextServer.AUTOWRAP_WORD_SMART
		hint.text = "Settings RPC unavailable (backend older than A2).\n\n" + \
			"Model: " + (_model_current if _model_current != "" else "—")
		hint.add_theme_font_size_override("font_size", 13)
		vbox.add_child(hint)
		return

	for i in namespaces.size():
		var ns: Dictionary = namespaces[i] as Dictionary
		if ns.is_empty():
			continue
		var header := Label.new()
		header.add_theme_font_size_override("font_size", 13)
		var writable: bool = bool(ns.get("writable", false))
		header.text = "Namespace: " + str(ns.get("ns", "?")) + \
			("  [writable]" if writable else "  [read-only]") + \
			"  rev " + str(ns.get("revision", 0))
		vbox.add_child(header)

		var doc := RichTextLabel.new()
		doc.bbcode_enabled = true
		doc.fit_content = true
		doc.scroll_active = false
		doc.autowrap_mode = TextServer.AUTOWRAP_WORD_SMART
		doc.text = _settings_doc_bb(ns)
		vbox.add_child(doc)

		# 可写：为每个顶层字段提供 LineEdit + Apply（经 settings.mutate set/unset）
		if writable:
			var fields: Dictionary = ns.get("schema", {})
			if fields is Dictionary:
				for key in fields.keys():
					if not (fields[key] is Dictionary):
						continue
					var row := HBoxContainer.new()
					var lbl := Label.new()
					lbl.text = str(key) + ":"
					lbl.custom_minimum_size = Vector2(90, 0)
					row.add_child(lbl)
					var edit := LineEdit.new()
					edit.placeholder_text = str(fields[key].get("value", ""))
					edit.text = str(fields[key].get("value", ""))
					edit.size_flags_horizontal = Control.SIZE_EXPAND_FILL
					row.add_child(edit)
					var apply := Button.new()
					apply.text = "Set"
					row.add_child(apply)
					var unset := Button.new()
					unset.text = "Unset"
					row.add_child(unset)
					vbox.add_child(row)
					_settings_bind_edit(ns.get("ns", ""), str(key), edit, apply, unset)

	# 底部通用提交行（批量 ops）
	if _settings_ns != "" and _settings_ops.size() > 0:
		var commit_row := HBoxContainer.new()
		var commit := Button.new()
		commit.text = "Commit changes"
		commit_row.add_child(commit)
		vbox.add_child(commit_row)
		commit.pressed.connect(func():
			var ns := _settings_ns
			var ops := _settings_ops.duplicate()
			client.settings_mutate(ns, ops, func(ok, resp):
				_settings_ops = []
				if ok:
					client.settings_describe(func(_ok2, d2): _populate_settings(d2))
			)
		)

func _settings_doc_bb(ns: Dictionary) -> String:
	var base: String = str(ns.get("base", "—"))
	var user: String = str(ns.get("user", ""))
	var schema: Variant = ns.get("schema", {})
	var bb := "[i]base:[/i] " + _esc_bb(base)
	if user != "":
		bb += "\n[i]user override:[/i] " + _esc_bb(user)
	bb += "\n[i]schema:[/i]\n"
	if schema is Dictionary:
		bb += _esc_bb(JSON.stringify(schema, "\t", false))
	else:
		bb += _esc_bb(str(schema))
	return bb

func _settings_bind_edit(ns: String, key: String, edit: LineEdit, apply: Button, unset: Button) -> void:
	# 编辑目标：收集到 _settings_ops，命中末行"Save changes"统一提交
	apply.pressed.connect(func():
		var path := key
		var val: Variant = edit.text
		var as_float := float(edit.text)
		var as_int := int(edit.text)
		if _looks_number(edit.text):
			if str(as_int) == edit.text:
				val = as_int
			else:
				val = as_float
		_settings_ops.append({"op": "set", "path": path, "value": val})
		_settings_ns = ns
	)
	unset.pressed.connect(func():
		_settings_ops.append({"op": "unset", "path": key})
		_settings_ns = ns
	)

func _looks_number(s: String) -> bool:
	if s == "":
		return false
	if not (s.is_valid_int() or s.is_valid_float()):
		return false
	return true

# ---- B5 settings 补充项：agent preset / permission presets / theme ----

## 在 settings 面板尾部追加补充配置行（agent preset、permission presets、theme）。
## 每次打开面板时重建，读取后端可经 session.command 逐项应用。
func _append_settings_extras() -> void:
	var vbox := settings_panel.get_node("Margin/V") as VBoxContainer
	# 分隔线
	var sep := HSeparator.new()
	vbox.add_child(sep)

	# --- agent preset ---
	var agent_row := HBoxContainer.new()
	var agent_lbl := Label.new()
	agent_lbl.text = "Agent preset:"
	agent_lbl.custom_minimum_size = Vector2(120, 0)
	agent_row.add_child(agent_lbl)
	var agent_opt := OptionButton.new()
	agent_opt.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	for p in ["default", "developer", "architect", "researcher"]:
		agent_opt.add_item(p)
	agent_opt.select(0)
	agent_row.add_child(agent_opt)
	vbox.add_child(agent_row)

	# --- permission presets ---
	var perm_lbl := Label.new()
	perm_lbl.text = "Permission preset:"
	perm_lbl.add_theme_font_size_override("font_size", 13)
	vbox.add_child(perm_lbl)
	var perm_row := HBoxContainer.new()
	perm_row.add_theme_constant_override("separation", 4)
	for p in ["default", "strict", "unrestricted"]:
		var b := Button.new()
		b.text = p
		b.pressed.connect(_on_permission_preset.bind(p))
		perm_row.add_child(b)
	vbox.add_child(perm_row)

	# --- theme / appearance ---
	var theme_row := HBoxContainer.new()
	var theme_lbl := Label.new()
	theme_lbl.text = "Theme:"
	theme_lbl.custom_minimum_size = Vector2(120, 0)
	theme_row.add_child(theme_lbl)
	var theme_opt := OptionButton.new()
	theme_opt.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	for t in ["System", "Dark", "Light"]:
		theme_opt.add_item(t)
	theme_opt.select(0)
	theme_opt.item_selected.connect(_on_theme_selected)
	theme_row.add_child(theme_opt)
	vbox.add_child(theme_row)

## permission preset 按钮：经 session.command 发 /permission <preset>（后端已注册）。
func _on_permission_preset(preset: String) -> void:
	if current_session_id == "":
		return
	client.send_command(current_session_id, "/permission " + preset, func(ok, resp):
		if ok and resp is Dictionary:
			_add_system_message(str(resp.get("text", "permission preset applied")))
		else:
			_add_system_message("permission preset failed")
	)

## theme 选择：本地 theme 开关（System/Dark/Light），前端观感，不接后端。
func _on_theme_selected(index: int) -> void:
	# 记录当前 theme 选择；具体外观应用由各主题资源渲染（当前为观感占位）。
	_theme_choice = index

# ---- session.context：上下文占用度量（A2 契约） ----

## 消费后端 session.context 的 contextPressure 展示到状态栏。
## 切换会话后调用一次，供用户观察上下文占用。
func _refresh_context_pressure() -> void:
	if current_session_id == "":
		return
	client.session_context(current_session_id, func(ok, data):
		if not (ok and data is Dictionary):
			return
		var pressure: float = float(data.get("contextPressure", 0.0))
		var limit: int = int(data.get("contextLimit", 0))
		var used: int = int(data.get("projectedTokens", 0))
		var pct := int(pressure * 100.0)
		# 状态栏右侧追加上下文占用（不覆盖主状态文本）
		status_label.text = "ctx %d%% (%d/%d)" % [pct, used, limit]
	)
