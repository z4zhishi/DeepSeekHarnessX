extends PanelContainer

const THEME_FILE := "user://theme.txt"
const TURN_WATCHDOG_SEC := 90.0

@onready var _client: Node = %DshClient
@onready var _store: Node = %SessionStore
@onready var _sidebar: SidebarPane = %Sidebar
@onready var _center: PanelContainer = %Center
@onready var _header: HeaderBar = %Header
@onready var _chat_tab: VBoxContainer = %ChatTab
@onready var _chat: Node = %ChatList
@onready var _composer: ComposerBar = %Composer
@onready var _hero: HeroView = %Hero
@onready var _traj: Control = %TrajectoryTab
@onready var _details: DetailsPane = %Details
@onready var _approval: Node = %ApprovalOverlay
@onready var _settings: Node = %SettingsOverlay
@onready var _onboarding: Node = %OnboardingOverlay
@onready var _jobs: Node = %JobsOverlay
@onready var _plugins: Node = %PluginsOverlay

var _cwd := ""
var _dark := true
var _sidebar_pref := DshTokens.SIDEBAR_DEFAULT
var _details_pref := 0.0
var _narrow := false
var _narrow_expanded := false
var _applied_cols: Dictionary = {}
var _drafts := {}
var _switching := false
var _ws_picker: DshFilePicker = null
var _watchdog: Timer = null
var _cols_timer: Timer = null
var _watch_session := ""
var _history_epoch: int = 0
var _mesh_a: TextureRect = null
var _mesh_b: TextureRect = null

func _ready() -> void:
	DisplayServer.window_set_title("DSHX")
	_cwd = _default_cwd()
	_dark = _load_theme_dark()
	_apply_theme(_dark)
	_client.session_event_received.connect(_on_session_event)
	_client.host_event_received.connect(_on_host_event)
	_client.connection_state_changed.connect(_on_connection)
	_bind(_client, "mux_ready", _on_mux_ready)
	_watchdog = Timer.new()
	_watchdog.one_shot = true
	_watchdog.wait_time = TURN_WATCHDOG_SEC
	_watchdog.timeout.connect(_on_turn_watchdog)
	add_child(_watchdog)
	# 列布局调度走单发 Timer（下一帧 _process 触发），不走 call_deferred：
	# _apply_columns 写 min size 会触发 resized，若在同一轮 MessageQueue
	# flush 内续链会形成自馈环把主循环饿死（历史事故：点收起侧栏整页卡死）。
	_cols_timer = Timer.new()
	_cols_timer.one_shot = true
	_cols_timer.wait_time = 0.02
	_cols_timer.timeout.connect(_apply_columns)
	add_child(_cols_timer)
	_store.sessions_changed.connect(_on_sessions_changed)
	_store.active_session_changed.connect(_on_active_changed)
	if _store.has_signal("lineage_changed"):
		_store.lineage_changed.connect(_on_lineage)
	_sidebar.new_session_pressed.connect(_on_new_session)
	_sidebar.session_selected.connect(_on_session_picked)
	_sidebar.workspace_pick_pressed.connect(_pick_workspace)
	_sidebar.settings_pressed.connect(_open_settings)
	_bind(_sidebar, "plugins_pressed", _open_plugins)
	_bind(_sidebar, "session_rename_requested", _on_session_rename_requested)
	_bind(_sidebar, "session_delete_requested", _on_session_delete_requested)
	_sidebar.theme_toggled.connect(_toggle_theme)
	_sidebar.collapse_pressed.connect(_toggle_sidebar)
	# 工作区选择器：应用内常驻预实例化（非原生对话框），首点零冷启动，
	# 主题随主窗口自动一致；最近工作区记录在 picker 的 "workspace" bucket。
	_ws_picker = DshFilePicker.new()
	_ws_picker.bucket = "workspace"
	_ws_picker.quick_dirs = [
		{ "label": _t("picker.home", "Home"), "path": DshFilePicker.home_dir() },
	]
	_ws_picker.dir_selected.connect(func(dir: String) -> void:
		if dir == "":
			return
		_cwd = dir.replace("\\", "/")
		_sidebar.set_workspace_label(_cwd.get_file() if _cwd.get_file() != "" else _cwd)
	)
	add_child(_ws_picker)
	_header.tab_changed.connect(_on_tab)
	_bind(_header, "param_effort_changed", _on_param_effort)
	_header.jobs_pressed.connect(_open_jobs)
	_header.model_selected.connect(_on_model)
	_composer.prompt_submitted.connect(_on_prompt)
	_composer.stop_requested.connect(_on_stop)
	_composer.command_submitted.connect(_on_command)
	_bind(_composer, "model_selected", _on_model)
	_bind(_composer, "access_mode_requested", _on_access_mode)
	_hero.suggestion_clicked.connect(_on_suggestion)
	_details.close_requested.connect(_close_details)
	_bind(_chat, "tool_selected", _on_tool_selected)
	_bind(_chat, "feedback_rating", _on_feedback)
	if _chat.has_method("show_hero"):
		_chat.show_hero(false)
	_bind(_chat, "suggestion_clicked", _on_suggestion)
	_bind(_approval, "decision_made", _on_approval)
	_bind(_settings, "theme_changed", _on_settings_theme)
	if _settings.has_method("setup"):
		_settings.setup(_client)
	if _jobs.has_method("setup"):
		_jobs.setup(_client)
	if _plugins != null and _plugins.has_method("setup"):
		_plugins.setup(_client)
	if _onboarding.has_method("setup"):
		_onboarding.setup(_client)
	resized.connect(_on_resized)
	_sidebar.set_workspace_label(_cwd.get_file() if _cwd != "" else _cwd)
	_show_empty(true)
	_request_columns()
	_client.describe(func(ok: bool, _d: Variant) -> void:
		_sidebar.set_status(_t("app.ready", "Ready") if ok else _t("app.backendOffline", "Backend offline"), ok)
	)
	_load_models()
	_refresh_commands()
	_restore_language()
	if _onboarding.has_method("maybe_start"):
		_onboarding.maybe_start(_client)
	else:
		_client.provider_describe(func(ok: bool, data: Variant) -> void:
			if ok and data is Dictionary and not bool(data.get("usable", true)) and _onboarding.has_method("open"):
				_onboarding.open()
		)
	_client.list_sessions(func(ok: bool, data: Variant) -> void:
		var arr := _as_array(data)
		if ok and not arr.is_empty():
			_store.set_sessions(arr)
			var first := _id_of(arr[0])
			if first != "":
				_switch(first, false)
		else:
			_create_session()
	)


func _bind(obj: Object, sig: String, cb: Callable) -> void:
	if obj != null and obj.has_signal(sig) and not obj.is_connected(sig, cb):
		obj.connect(sig, cb)


func _on_resized() -> void:
	_request_columns()


func _unhandled_input(event: InputEvent) -> void:
	var k := event as InputEventKey
	if k == null or not k.pressed or k.echo:
		return
	var ctrl := k.ctrl_pressed or k.meta_pressed
	if ctrl:
		match k.keycode:
			KEY_N:
				_on_new_session()
				get_viewport().set_input_as_handled()
				return
			KEY_F:
				if _sidebar.has_method("focus_search"):
					_sidebar.focus_search()
				elif _search_focus_fallback():
					pass
				get_viewport().set_input_as_handled()
				return
			KEY_J:
				_open_jobs()
				get_viewport().set_input_as_handled()
				return
			KEY_COMMA:
				_open_settings()
				get_viewport().set_input_as_handled()
				return
	if k.keycode == KEY_ESCAPE:
		if _approval != null and _approval.visible:
			if _approval.has_method("cancel_open"):
				_approval.cancel_open()
			elif _approval.has_method("hide_request"):
				_approval.hide_request()
			else:
				_approval.visible = false
			get_viewport().set_input_as_handled()
			return
		# Do not steal composer Esc when composer has focus and generating.
		if _composer != null and _composer.has_method("is_generating") and bool(_composer.call("is_generating")):
			var focused := get_viewport().gui_get_focus_owner()
			if focused != null and _composer.is_ancestor_of(focused):
				return
		if _details != null and _details.visible and _details_pref > 0.0:
			_close_details()
			get_viewport().set_input_as_handled()
			return
		if _composer != null and _composer.has_method("is_generating") and bool(_composer.call("is_generating")):
			_on_stop()
			get_viewport().set_input_as_handled()
			return
		# else clear: clear composer draft if any, otherwise mark handled
		if _composer != null and _composer.has_method("get_draft"):
			var draft := str(_composer.call("get_draft")).strip_edges()
			if draft != "":
				_composer.set_draft("")
				get_viewport().set_input_as_handled()
				return
		get_viewport().set_input_as_handled()
		return


func _search_focus_fallback() -> bool:
	if _sidebar != null and _sidebar.has_node("%SessionSearch"):
		var n := _sidebar.get_node_or_null("%SessionSearch")
		if n is Control:
			(n as Control).grab_focus()
			if n is LineEdit:
				(n as LineEdit).select_all()
			return true
	return false


func _request_columns() -> void:
	if not _cols_timer.is_stopped():
		return
	_cols_timer.start()


func _apply_theme(dark: bool) -> void:
	_dark = dark
	theme = DshThemeBuilder.build(dark)
	# OLED 底 + 双 radial mesh 光晕：两枚大半径渐变贴在 Center 背景上，
	# 打破纯平灰黑（Ethereal Glass 基调，token 见 tokens.bg_mesh_a/b）。
	var base := DshTokens.box(DshTokens.bg_base(), 0, Color.TRANSPARENT, 0, Vector4.ZERO)
	base.shadow_color = Color(0, 0, 0, 0)
	_center.add_theme_stylebox_override("panel", base)
	_paint_mesh()
	_sidebar.apply_tokens()
	_header.apply_tokens()
	_composer.apply_tokens()
	_details.apply_tokens()
	_hero.apply_tokens()


## 两枚 radial 光晕：左上钴蓝、右下青。用 GradientTexture2D radial fill +
## TextureRect（stretch keep covered），pointer ignore 不挡命中。
## 只在 _apply_theme（_ready / 主题切换）调用，绝不在 _process 链上 add_child；
## 每次切换都重建（不短路 return），否则 _mesh_a 指向已 queue_free 的旧节点，
## 主题切换后光晕不会更新。
func _paint_mesh() -> void:
	for old in _center.get_children():
		if old.name.begins_with("MeshGlow"):
			_center.remove_child(old)
			old.queue_free()
	_mesh_a = _make_glow(DshTokens.bg_mesh_a(), Vector2(0.12, 0.08), 0.10)
	_mesh_b = _make_glow(DshTokens.bg_mesh_b(), Vector2(0.92, 0.95), 0.07)


func _make_glow(color: Color, anchor: Vector2, strength: float) -> TextureRect:
	var grad := Gradient.new()
	var c := color
	c.a = clampf(strength * (1.6 if DshTokens.is_dark() else 1.2), 0.02, 0.35)
	grad.set_color(0, c)
	var c_end := color
	c_end.a = 0.0
	grad.set_color(1, c_end)
	var tex := GradientTexture2D.new()
	tex.gradient = grad
	tex.fill = GradientTexture2D.FILL_RADIAL
	tex.fill_from = Vector2(0.5, 0.5)
	tex.fill_to = Vector2(0.5, 0.0)
	tex.width = 512
	tex.height = 512
	var rect := TextureRect.new()
	rect.name = "MeshGlow_%s_%s" % [str(int(anchor.x * 100)), str(int(anchor.y * 100))]
	rect.texture = tex
	rect.expand_mode = TextureRect.EXPAND_IGNORE_SIZE
	rect.stretch_mode = TextureRect.STRETCH_SCALE
	rect.mouse_filter = Control.MOUSE_FILTER_IGNORE
	rect.set_anchors_preset(Control.PRESET_FULL_RECT)
	rect.offset_left = -256 + anchor.x * 1280
	rect.offset_top = -256 + anchor.y * 800
	rect.offset_right = rect.offset_left + 1024
	rect.offset_bottom = rect.offset_top + 1024
	_center.add_child(rect)
	_center.move_child(rect, 0)
	return rect


func _toggle_theme() -> void:
	_apply_theme(not _dark)
	_save_theme(_dark)


func _on_settings_theme(is_dark: bool) -> void:
	_apply_theme(is_dark)
	_save_theme(is_dark)


func _load_theme_dark() -> bool:
	if FileAccess.file_exists(THEME_FILE):
		return FileAccess.get_file_as_string(THEME_FILE).strip_edges() != "light"
	return true


func _save_theme(dark: bool) -> void:
	var f := FileAccess.open(THEME_FILE, FileAccess.WRITE)
	if f:
		f.store_string("dark" if dark else "light")
		f.close()


func _apply_columns() -> void:
	_udbg("columns enter vp=%.1f" % size.x)
	var vp := size.x
	var narrow := vp < DshTokens.SIDEBAR_AUTO_COLLAPSE
	if narrow != _narrow:
		_narrow = narrow
		_narrow_expanded = false
	var sidebar_pref := 0.0
	var rail := (_narrow and not _narrow_expanded) or (not _narrow and _sidebar_pref == 0.0)
	if not rail:
		sidebar_pref = _sidebar_pref if _sidebar_pref > 0.0 else DshTokens.SIDEBAR_DEFAULT
	var cols := DshColumns.compute_columns(vp, sidebar_pref, _details_pref)
	# 幂等写入：值未变化的属性绝不重写。min size / visible 的无条件重写会
	# 触发 layout+resized 信号，是列布局自馈环的燃料。
	var sb: float = cols.sidebar
	var ct: float = cols.center
	var dt: float = cols.details
	if not is_equal_approx(_applied_cols.get("sidebar", -1.0), sb):
		_applied_cols["sidebar"] = sb
		_sidebar.custom_minimum_size.x = sb
	if not is_equal_approx(_applied_cols.get("center", -1.0), ct):
		_applied_cols["center"] = ct
		_center.custom_minimum_size.x = ct
	var sb_collapsed: bool = sb <= DshColumns.SIDEBAR_COLLAPSED + 0.5
	_sidebar.set_collapsed(sb_collapsed)
	var open: bool = dt > 0.0
	if (not _applied_cols.has("details_open") or _applied_cols["details_open"] != open) and _details.visible != open:
		_details.visible = open
	_applied_cols["details_open"] = open
	if not is_equal_approx(_applied_cols.get("details", -1.0), dt):
		_applied_cols["details"] = dt
		_details.custom_minimum_size.x = dt
	_details.set_collapsed(not open)
	_udbg("columns exit s=%.0f c=%.0f d=%.0f" % [sb, ct, dt])


func _toggle_sidebar() -> void:
	_udbg("toggle enter narrow=%s pref=%.1f" % [_narrow, _sidebar_pref])
	if _narrow:
		_narrow_expanded = not _narrow_expanded
	else:
		_sidebar_pref = 0.0 if _sidebar_pref != 0.0 else DshTokens.SIDEBAR_DEFAULT
	_apply_columns()
	_udbg("toggle exit")


func _udbg(m: String) -> void:
	if OS.get_environment("DSHX_UI_DEBUG") == "":
		return
	var line := "[%9.3f f=%d] app %s" % [Time.get_ticks_msec() / 1000.0, Engine.get_process_frames(), m]
	print(line)
	var f := FileAccess.open("user://app_log.txt", FileAccess.READ_WRITE)
	if f == null:
		f = FileAccess.open("user://app_log.txt", FileAccess.WRITE)
	if f:
		f.seek_end()
		f.store_line(line)
		f.close()


func _on_tab(name: String) -> void:
	_chat_tab.visible = name == "chat"
	_traj.visible = name == "trajectory"


func _on_connection(connected: bool) -> void:
	_sidebar.set_status(_t("app.connected", "Connected") if connected else _t("app.disconnected", "Disconnected"), connected)


func _on_sessions_changed(_payload: Variant = null) -> void:
	_sidebar.set_sessions(_store.sessions, _active_id())


func _on_active_changed(id: String) -> void:
	_sidebar.set_sessions(_store.sessions, id)
	_header.set_title(_title_of(id))
	_header.set_lineage(_lineage_of(id))


func _on_lineage(parts: Array) -> void:
	var titles := PackedStringArray()
	for p in parts:
		if p is Dictionary:
			titles.append(str(p.get("title", p.get("id", ""))))
		else:
			titles.append(str(p))
	_header.set_lineage(titles)


func _on_new_session() -> void:
	_create_session()


func _on_session_picked(id: String) -> void:
	_switch(id, false)


func _create_session() -> void:
	_client.create_session(_cwd, "default", func(ok: bool, data: Variant) -> void:
		if not ok:
			return
		var header: Dictionary = data if data is Dictionary else {"id": _id_of(data), "cwd": _cwd}
		if str(header.get("id", "")) == "":
			header["id"] = _id_of(data)
		if str(header.get("cwd", "")) == "":
			header["cwd"] = _cwd
		_store.upsert_session(header)
		var id := _id_of(header)
		if id != "":
			_switch(id, true)
	)


func _switch(id: String, is_new: bool) -> void:
	if id == "" or _switching:
		return
	_switching = true
	_save_draft()
	_store.set_active(id)
	_client.set_active_session(id)
	if _chat.has_method("clear"):
		_chat.clear()
	if _traj.has_method("clear_all"):
		_traj.clear_all()
	_disarm_watchdog()
	_composer.set_generating(false)
	_composer.set_enabled(true)
	_composer.set_draft(str(_drafts.get(id, "")))
	_close_details()
	_show_empty(true)
	_header.set_title(_title_of(id))
	_header.set_lineage(_lineage_of(id))
	_header.set_plan_active(false)
	var cwd := str(_store.get_session(id).get("cwd", _cwd))
	if cwd != "":
		_cwd = cwd
		_sidebar.set_workspace_label(cwd.get_file() if cwd.get_file() != "" else cwd)
	# 递增 epoch 使旧会话的异步回调失效。每次切换或 mux 重连都递增，
	# 确保只有最新一轮加载的回调能写入 ChatList。
	_history_epoch += 1
	_switching = false
	_composer.grab_input_focus()
	if is_new:
		return
	# 历史加载统一由 _on_mux_ready 驱动（WS 连接就绪后）。
	# 不在这里调 resume + fetch_history，避免与 mux_ready 形成
	# 双路投递，叠加 WS replay 成三路——这是旧对话闪烁的根因。


func _apply_history(ok: bool, data: Variant) -> void:
	var events := _as_array(data)
	if _traj.has_method("set_events"):
		_traj.set_events(events)
	if not ok:
		return
	if events.is_empty():
		var live := _chat.has_method("is_empty") and not bool(_chat.call("is_empty"))
		if not live:
			_show_empty(true)
			if _chat.has_method("clear"):
				_chat.clear()
		return
	_show_empty(false)
	if _chat.has_method("set_nodes"):
		# set_nodes adopts the folded nodes and resets ChatList's internal fold,
		# so live mux events after this point re-fold from a clean baseline.
		var fold := ConversationFold.new()
		fold.ingest_history(events)
		_chat.set_nodes(fold.nodes(), fold.seen_seq())


func _on_mux_ready(session_id: String) -> void:
	if session_id == "" or session_id != _active_id():
		return
	_refresh_commands()
	# 唯一的历史加载入口。先拿完整日志（fetch_history），再唤醒 agent
	# （resume）。顺序反转保证：取历史时 agent 尚未产生新事件，WS
	# 通道不会在 history 回调到达前推送实时事件造成交错。
	var epoch := _history_epoch
	_client.fetch_history(session_id, 0, func(ok: bool, events: Variant) -> void:
		# epoch 不匹配说明用户已切到其他会话，丢弃本轮结果。
		if _history_epoch != epoch or _active_id() != session_id:
			return
		_apply_history(ok, events)
		_refresh_context()
		# 历史到位后再唤醒 agent。
		_client.resume_session(session_id, func(_ok2: bool, _d2: Variant) -> void:
			_read_policy_back(session_id)
		)
	)


func _on_session_event(event: Dictionary) -> void:
	_show_empty(false)
	if _watch_session == _active_id() and _watchdog != null and not _watchdog.is_stopped():
		_arm_watchdog(_watch_session)
	if _chat.has_method("apply_event"):
		_chat.apply_event(event)
	if _traj.has_method("append_event"):
		_traj.append_event(event)
	var kind := str(event.get("type", ""))
	var data: Dictionary = event.get("data", {}) if event.get("data") is Dictionary else {}
	match kind:
		"turn/start":
			_composer.set_generating(true)
		"turn/end":
			_composer.set_generating(false)
			_disarm_watchdog()
			_refresh_context()
			var reason: Dictionary = data.get("reason", {}) if data.get("reason") is Dictionary else {}
			if str(reason.get("kind", "")) == "error":
				var code := str(reason.get("code", ""))
				var msg := str(reason.get("message", "")).strip_edges()
				if msg == "":
					msg = _t("chat.systemFailed", "Failed to send prompt.")
				if code == "AUTH":
					# Auth / authorization failure: a blocking modal steers the user
					# to the provider credentials instead of an inline row.
					_inject_system_line(_t("app.authError", "Authentication failed. Please configure your API key in Settings.") + "\n" + msg)
					_open_settings()
				elif msg.find("MISSING_CREDENTIAL") >= 0 or msg.to_lower().find("no api key") >= 0:
					_inject_system_line(_t("app.missingCredential", "No API key configured — configure a provider in Settings."))
				else:
					# Parameter / config failures (e.g. max_tokens over the model cap,
					# INVALID_REQUEST): inline, non-blocking hint + a follow-up line
					# pointing at Settings — no modal blocks the prompt editor.
					_inject_turn_error(msg)
					if code == "INVALID_REQUEST" or code == "param_mismatch":
						_inject_system_line(_t("app.paramMismatchHint", "Invalid request — check the model/parameters. Open Settings to adjust."))
		"assistant/message":
			# Do not drop generating here: the actor still runs tools after
			# assistant/message. Only turn/end is the idle edge.
			_refresh_context()
		"plan/mode":
			_header.set_plan_active(bool(data.get("active", false)))
		"session/title":
			var title := str(data.get("title", ""))
			var sid := _active_id()
			if title != "" and sid != "":
				_store.upsert_session({"id": sid, "title": title})
				_header.set_title(title)


func _on_host_event(method: String, payload: Variant) -> void:
	var p: Dictionary = payload if payload is Dictionary else {}
	_sidebar.handle_host_event(method, payload)
	match method:
		"host/session-added":
			_store.upsert_session(p)
		"host/session-deleted":
			var del_id := str(p.get("id", p.get("sessionId", "")))
			if del_id != "":
				if _store.has_method("remove_session"):
					_store.remove_session(del_id)
				if del_id == _active_id():
					_show_empty(true)
					_header.set_title("")
		"host/subagent-started":
			var child := str(p.get("childSessionId", ""))
			var parent := str(p.get("parentSessionId", ""))
			if child != "":
				_store.upsert_session({"id": child, "parentSession": parent})
		"host/permission-request":
			if _approval.has_method("show_request"):
				_approval.show_request(str(p.get("callId", "")), str(p.get("prompt", "")), p.get("options", []))
		"host/permission-resolved":
			# 网关侧审批已成终态（超时取消 / 其他端已决）。转发 overlay 关闭
			# 匹配卡片，并把 outcome 写一行 system 文案进聊天流。网关侧帧
			# 未上线期间此分支静默不触发；overlay 缺 resolve_remote 时跳过。
			var call_id := str(p.get("callId", ""))
			if call_id != "":
				if _approval.has_method("resolve_remote"):
					_approval.resolve_remote(call_id, str(p.get("outcome", "")))
				_inject_system_line(_approval_resolved_text(str(p.get("outcome", "cancel"))))
		"settings/updated":
			_load_models()


func _on_prompt(text: String, _attachments: Array) -> void:
	var steering := _composer.has_method("is_generating") and bool(_composer.call("is_generating"))
	_ensure_session(func(id: String) -> void:
		_show_empty(false)
		if steering:
			_steer_or_prompt(id, text)
			return
		_composer.set_generating(true)
		_arm_watchdog(id)
		_client.send_prompt(id, text, func(ok: bool, data: Variant) -> void:
			if not ok:
				_fail_turn(data)
		)
	)


func _steer_or_prompt(id: String, text: String) -> void:
	if _client.has_method("steer_prompt"):
		_client.steer_prompt(id, text, func(ok: bool, _data: Variant) -> void:
			if ok:
				_arm_watchdog(id)
				return
			_client.send_prompt(id, text, func(ok2: bool, data2: Variant) -> void:
				if ok2:
					_arm_watchdog(id)
					return
				_inject_turn_error(_rpc_error_text(data2))
			)
		)
		return
	_client.send_prompt(id, text, func(ok: bool, data: Variant) -> void:
		if ok:
			_arm_watchdog(id)
			return
		_inject_turn_error(_rpc_error_text(data))
	)


func _on_command(line: String) -> void:
	_ensure_session(func(id: String) -> void:
		_show_empty(false)
		_client.send_command(id, line, func(ok: bool, data: Variant) -> void:
			if not ok:
				_inject_turn_error(_rpc_error_text(data))
		)
	)


func _on_stop() -> void:
	_disarm_watchdog()
	_composer.set_generating(false)
	if _approval != null and _approval.visible:
		if _approval.has_method("cancel_open"):
			_approval.cancel_open()
	var id := _active_id()
	if id != "":
		if _client.has_method("abort_session"):
			_client.abort_session(id, func(_ok: bool, _d: Variant) -> void: pass)
		else:
			_client.stop_session(id, func(_ok: bool, _d: Variant) -> void: pass)


func _on_suggestion(prompt: String) -> void:
	if prompt.begins_with("/"):
		_on_command(prompt)
	else:
		_on_prompt(prompt, [])


func _on_tool_selected(_call_id: String, name: String, input: Variant, output: Variant) -> void:
	_details_pref = DshTokens.DETAILS_DEFAULT
	_details.show_tool(name, _as_text(input), _as_text(output))
	_apply_columns()


func _close_details() -> void:
	_details_pref = 0.0
	_apply_columns()


func _on_model(id: String) -> void:
	_client.set_model(id, func(_ok: bool, _d: Variant) -> void: pass)


func _on_param_effort(effort: Variant) -> void:
	var e: String = str(effort).strip_edges()
	if e == "":
		return
	_ensure_session(func(id: String) -> void:
		_client.session_effort(id, e, func(_ok: bool, data: Variant) -> void:
			if data is Dictionary:
				_header.set_effort(str((data as Dictionary).get("effort", e)))
		)
	)


func _on_access_mode(preset: String) -> void:
	var p := preset.strip_edges()
	if p == "":
		return
	_ensure_session(func(id: String) -> void:
		_client.send_command(id, "/permission " + p, func(ok: bool, data: Variant) -> void:
			if not ok:
				_inject_turn_error(_rpc_error_text(data))
				return
			_composer.set_access_mode(p)
		)
	)


func _on_approval(call_id: String, decision: String) -> void:
	_client.respond_approval(call_id, decision, func(_ok: bool, _d: Variant) -> void: pass)


func _on_feedback(message_id: String, rating: String) -> void:
	if _client.has_method("feedback_put"):
		_client.feedback_put(_active_id(), message_id, rating, "", "", func(_ok: bool, _d: Variant) -> void: pass)


func _open_settings() -> void:
	if _settings.has_method("open_panel"):
		_settings.open_panel()
	elif _settings.has_method("open"):
		_settings.open()


func _open_jobs() -> void:
	var id := _active_id()
	if _jobs.has_method("open_for"):
		_jobs.open_for(id)
	else:
		if _jobs.has_method("set_session"):
			_jobs.set_session(id)
		if _jobs.has_method("open"):
			_jobs.open()


func _open_plugins() -> void:
	if _plugins == null:
		return
	if _plugins.has_method("open_panel"):
		_plugins.open_panel()
	elif _plugins.has_method("open"):
		_plugins.open()


func _refresh_commands() -> void:
	if not _client.has_method("list_commands"):
		return
	_client.list_commands(func(ok: bool, data: Variant) -> void:
		if not ok:
			return
		var cmds := _as_array(data)
		if cmds.is_empty() and data is Dictionary and (data as Dictionary).get("commands") is Array:
			cmds = (data as Dictionary)["commands"]
		if _composer.has_method("set_commands"):
			_composer.set_commands(cmds)
	)


func _arm_watchdog(session_id: String) -> void:
	_watch_session = session_id
	if _watchdog != null:
		_watchdog.start(TURN_WATCHDOG_SEC)


func _disarm_watchdog() -> void:
	_watch_session = ""
	if _watchdog != null:
		_watchdog.stop()


func _on_turn_watchdog() -> void:
	if _watch_session == "" or _watch_session != _active_id():
		return
	if not (_composer.has_method("is_generating") and bool(_composer.call("is_generating"))):
		return
	_composer.set_generating(false)
	_inject_turn_error(_t("chat.turnTimeout", "No response from the agent. Generation stopped."))
	_watch_session = ""


func _fail_turn(data: Variant) -> void:
	_disarm_watchdog()
	_composer.set_generating(false)
	_inject_turn_error(_rpc_error_text(data))


func _inject_system_line(text: String) -> void:
	# 本地下行文案走 system/notice 折叠通道；seq 取负数避免与持久化事件
	# 的正 seq 空间撞车（ConversationFold 以 seq 做幂等去重）。
	if text.strip_edges() == "":
		return
	_show_empty(false)
	if _chat.has_method("apply_event"):
		_chat.apply_event({
			"type": "system/notice",
			"seq": -Time.get_ticks_msec(),
			"data": {"text": text, "reason": "approval"},
		})


func _approval_resolved_text(outcome: String) -> String:
	# 词表键尚未入库（i18n.gd 不在本次改动范围）；按当前语言给内联回退，
	# 后续词表补齐后 _t 自动优先取词表。
	var zh: bool = get_node_or_null("/root/DshI18n") != null and DshI18n.is_zh()
	match outcome:
		"timeout", "cancel", "cancelled":
			return _t("app.approvalTimeout", "审批已超时取消" if zh else "Approval timed out and was cancelled")
		"deny", "denied":
			return _t("app.approvalDenied", "审批已被拒绝" if zh else "Approval was denied")
		"allow_once", "allow_all", "allow", "allowed":
			return _t("app.approvalAllowed", "审批已允许" if zh else "Approval was granted")
	return _t("app.approvalResolved", "审批已结束" if zh else "Approval resolved")


func _inject_turn_error(text: String) -> void:
	var msg := text.strip_edges()
	if msg == "":
		msg = _t("chat.systemFailed", "Failed to send prompt.")
	_show_empty(false)
	if _chat.has_method("apply_event"):
		_chat.apply_event({
			"type": "system/error",
			"seq": Time.get_ticks_msec(),
			"data": {"text": msg, "reason": "error"},
		})


func _rpc_error_text(data: Variant) -> String:
	if data is Dictionary:
		var err: Variant = (data as Dictionary).get("error", "")
		if err is Dictionary:
			var msg := str((err as Dictionary).get("message", (err as Dictionary).get("error", "")))
			if msg != "":
				return msg
		var s := str(err).strip_edges()
		if s != "" and s != "<null>":
			return s
	return _t("chat.systemFailed", "Failed to send prompt.")


func _ensure_session(then: Callable) -> void:
	var id := _active_id()
	if id != "":
		then.call(id)
		return
	_client.create_session(_cwd, "default", func(ok: bool, data: Variant) -> void:
		if not ok:
			return
		var header: Dictionary = data if data is Dictionary else {"id": _id_of(data), "cwd": _cwd}
		if str(header.get("id", "")) == "":
			header["id"] = _id_of(data)
		_store.upsert_session(header)
		var sid := _id_of(header)
		if sid == "":
			return
		_switch(sid, true)
		then.call(sid)
	)


## Backend is authoritative for general.language (settings.mutate on every
## locale change); user://locale.txt is only a fast first-paint guess.
func _restore_language() -> void:
	if not _client.has_method("settings_describe"):
		return
	_client.settings_describe(func(ok: bool, data: Variant) -> void:
		if not ok or not (data is Dictionary):
			return
		var loc := ""
		for ns_v in (data as Dictionary).get("namespaces", []):
			if not (ns_v is Dictionary) or str((ns_v as Dictionary).get("ns", "")) != "general":
				continue
			var ns: Dictionary = ns_v
			for src in ["user", "base"]:
				var m: Variant = ns.get(src, {})
				if m is Dictionary and str((m as Dictionary).get("language", "")) != "":
					loc = str((m as Dictionary)["language"])
		if (loc == "zh" or loc == "en") and loc != DshI18n.get_locale():
			DshI18n.set_locale(loc)
	)


func _load_models() -> void:
	if _client.has_method("provider_models"):
		_client.provider_models("", func(ok: bool, data: Variant) -> void:
			if ok:
				_apply_models(true, data)
				return
			_client.list_models(func(ok2: bool, data2: Variant) -> void: _apply_models(ok2, data2))
		)
	else:
		_client.list_models(func(ok: bool, data: Variant) -> void: _apply_models(ok, data))


func _apply_models(ok: bool, data: Variant) -> void:
	if not ok:
		return
	var models: Array = []
	var selected := ""
	if data is Dictionary:
		if data.get("models") is Array:
			models = data["models"]
		selected = str(data.get("selected", data.get("active", "")))
	elif data is Array:
		models = data
	_header.set_models(models, selected)
	if _composer.has_method("set_models"):
		_composer.set_models(models, selected)


func _refresh_context() -> void:
	var id := _active_id()
	if id == "":
		return
	_client.session_context(id, func(ok: bool, data: Variant) -> void:
		if not ok or not (data is Dictionary):
			return
		var p := float(data.get("contextPressure", 0.0))
		var used := int(data.get("tokenUsageInput", data.get("projectedTokens", 0)))
		var limit := int(data.get("contextLimit", 0))
		var pcts := int(round(clampf(p, 0.0, 1.0) * 100.0))
		# 自解释标签：百分比一眼可读；完整 token 数放悬停提示。
		var label := "%d%% ctx" % pcts
		var detail := _t("app.contextTipPct", "Context pressure: %d%% of the model window" % pcts)
		if limit > 0:
			detail = _t("app.contextTip", "Context: %d / %d tokens (%d%%)" % [used, limit, pcts])
		_header.set_context(p, label, detail)
	)


## _read_policy_back syncs the composer's access dropdown to the session's real
## permission mode (via session.policy). The backend is authoritative; the
## frontend never guesses, so a resumed/auto-created session shows its true mode
## instead of resetting to "default" (which is why /permission and the dropdown
## previously disagreed after every resume).
func _read_policy_back(session_id: String) -> void:
	if session_id == "":
		return
	if not _client.has_method("session_policy"):
		return
	_client.session_policy(session_id, func(ok: bool, data: Variant) -> void:
		if not ok or not (data is Dictionary):
			return
		if _active_id() != session_id:
			return
		var mode := str((data as Dictionary).get("mode", ""))
		if mode != "" and _composer.has_method("set_access_mode"):
			_composer.set_access_mode(mode)
	)


func _show_empty(empty: bool) -> void:
	_hero.visible = empty
	_chat.visible = not empty


func _save_draft() -> void:
	var id := _active_id()
	if id != "":
		_drafts[id] = _composer.get_draft()


func _pick_workspace() -> void:
	if _ws_picker == null:
		return
	_ws_picker.open({
		"mode": "dir",
		"title": _t("workspace.selectDir", "Select Workspace Directory"),
		"start_dir": _cwd,
	})


func _title_of(id: String) -> String:
	var s: Dictionary = _store.get_session(id)
	var title := str(s.get("title", ""))
	if title != "":
		return title
	var cwd := str(s.get("cwd", ""))
	if cwd != "":
		var base := cwd.get_file()
		return base if base != "" else cwd
	return id.substr(0, 8) if id.length() > 8 else id


func _lineage_of(id: String) -> PackedStringArray:
	var parts := PackedStringArray()
	var chain: Array = _store.breadcrumb(id) if _store.has_method("breadcrumb") else [id]
	for sid in chain:
		parts.append(_title_of(str(sid)))
	return parts


func _active_id() -> String:
	var g: Variant = _store.get_active()
	if g is Dictionary:
		return _id_of(g)
	return str(g)


func _as_array(data: Variant) -> Array:
	if data is Array:
		return data
	if data is Dictionary:
		for key in ["sessions", "events", "items", "value"]:
			if data.get(key) is Array:
				return data[key]
	return []


func _id_of(data: Variant) -> String:
	if data is Dictionary:
		var id := str(data.get("id", data.get("ID", "")))
		if id != "":
			return id
		if data.get("session") is Dictionary:
			return str(data["session"].get("id", ""))
	return str(data) if data is String else ""


func _as_text(v: Variant) -> String:
	if v is String:
		return v
	if v is Dictionary or v is Array:
		return JSON.stringify(v, "  ")
	return str(v)


func _on_session_rename_requested(id: String, title: String) -> void:
	if id == "" or title.strip_edges() == "":
		return
	var cb := func(ok: bool, data: Variant) -> void:
		if not ok:
			_inject_turn_error(_rpc_error_text(data))
			return
		_store.upsert_session({"id": id, "title": title.strip_edges()})
		if _active_id() == id:
			_header.set_title(title.strip_edges())
	if _client.has_method("session_rename"):
		_client.session_rename(id, title.strip_edges(), cb)
	else:
		_client._rpc("session.rename", {"sessionId": id, "title": title.strip_edges()}, cb)


func _on_session_delete_requested(id: String) -> void:
	if id == "":
		return
	var was_active := _active_id() == id
	var cb := func(ok: bool, data: Variant) -> void:
		if not ok:
			_inject_turn_error(_rpc_error_text(data))
			return
		if _store.has_method("remove_session"):
			_store.remove_session(id)
		else:
			_store.set_sessions(_store.sessions)
		if was_active:
			if _store.sessions.is_empty():
				_create_session()
			else:
				var next := _id_of(_store.sessions[0])
				if next != "":
					_switch(next, false)
	if _client.has_method("session_delete"):
		_client.session_delete(id, cb)
	else:
		_client._rpc("session.delete", {"sessionId": id}, cb)


func _default_cwd() -> String:
	var env := OS.get_environment("DSHX_WORKSPACE")
	if env != "":
		return env.replace("\\", "/")
	return ProjectSettings.globalize_path("res://").replace("\\", "/").get_base_dir()


func _t(key: String, fallback: String) -> String:
	var v := str(DshI18n.t(key))
	return fallback if v == key or v.strip_edges() == "" else v
