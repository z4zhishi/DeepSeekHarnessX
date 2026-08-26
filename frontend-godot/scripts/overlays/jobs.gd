extends CanvasLayer
class_name JobsOverlay

## Background jobs list, output pane, and kill. Polls while open.

var _client: DshClient = null
var _session_id: String = ""
var _jobs: Array = []
var _selected_id: String = ""

var _backdrop: ColorRect
var _card: PanelContainer
var _title: Label
var _list: ItemList
var _output: TextEdit
var _refresh_btn: Button
var _kill_btn: Button
var _kill_armed := false
var _kill_arm_timer: Timer
var _empty: Label
var _output_lbl: Label
var _poll: Timer


func _ready() -> void:
	layer = 20
	visible = false
	_build()
	_poll = Timer.new()
	_poll.wait_time = 2.0
	_poll.timeout.connect(_refresh)
	add_child(_poll)
	if get_node_or_null("/root/DshI18n") != null:
		DshI18n.locale_changed.connect(_apply_strings)


func setup(client: DshClient) -> void:
	if _client != null and _client.jobs_refreshed.is_connected(_on_jobs_refreshed):
		_client.jobs_refreshed.disconnect(_on_jobs_refreshed)
	_client = client
	if _client != null:
		_client.jobs_refreshed.connect(_on_jobs_refreshed)


func set_session(session_id: String) -> void:
	_session_id = session_id
	if visible:
		_refresh()


func open_for(session_id: String) -> void:
	set_session(session_id)
	open()


func open() -> void:
	visible = true
	_refresh()
	if _poll != null:
		_poll.start()


func close() -> void:
	visible = false
	if _poll != null:
		_poll.stop()


func _build() -> void:
	_backdrop = ColorRect.new()
	_backdrop.color = Color(0, 0, 0, 0.45)
	_backdrop.set_anchors_preset(Control.PRESET_FULL_RECT)
	_backdrop.mouse_filter = Control.MOUSE_FILTER_STOP
	_backdrop.gui_input.connect(_on_backdrop)
	add_child(_backdrop)

	var center := CenterContainer.new()
	center.set_anchors_preset(Control.PRESET_FULL_RECT)
	center.mouse_filter = Control.MOUSE_FILTER_IGNORE
	add_child(center)

	_card = PanelContainer.new()
	_card.custom_minimum_size = Vector2(640, 480)
	center.add_child(_card)

	var box := VBoxContainer.new()
	box.add_theme_constant_override("separation", 10)
	_card.add_child(box)

	var header := HBoxContainer.new()
	box.add_child(header)
	_title = Label.new()
	_title.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_title.add_theme_font_size_override("font_size", 16)
	header.add_child(_title)
	var close_btn := Button.new()
	close_btn.text = "×"
	close_btn.custom_minimum_size = Vector2(32, 28)
	close_btn.pressed.connect(close)
	header.add_child(close_btn)

	_list = ItemList.new()
	_list.size_flags_vertical = Control.SIZE_EXPAND_FILL
	_list.custom_minimum_size = Vector2(0, 160)
	_list.item_selected.connect(_on_item_selected)
	box.add_child(_list)

	_empty = Label.new()
	_empty.autowrap_mode = TextServer.AUTOWRAP_WORD_SMART
	box.add_child(_empty)

	_output_lbl = Label.new()
	box.add_child(_output_lbl)

	_output = TextEdit.new()
	_output.editable = false
	_output.wrap_mode = TextEdit.LINE_WRAPPING_BOUNDARY
	_output.size_flags_vertical = Control.SIZE_EXPAND_FILL
	_output.custom_minimum_size = Vector2(0, 160)
	_output.add_theme_font_override("font", DshThemeBuilder.code_font())
	box.add_child(_output)

	var btns := HBoxContainer.new()
	btns.add_theme_constant_override("separation", 8)
	btns.alignment = BoxContainer.ALIGNMENT_END
	box.add_child(btns)
	_refresh_btn = Button.new()
	_refresh_btn.pressed.connect(_refresh)
	btns.add_child(_refresh_btn)
	_kill_btn = Button.new()
	_kill_btn.pressed.connect(_kill_selected)
	btns.add_child(_kill_btn)
	# kill 防呆定时器：二次点击窗口超时后回到普通态（内联确认模式）。
	_kill_arm_timer = Timer.new()
	_kill_arm_timer.one_shot = true
	_kill_arm_timer.wait_time = 3.0
	_kill_arm_timer.timeout.connect(_disarm_kill)
	add_child(_kill_arm_timer)

	_apply_style()
	_apply_strings()


func _apply_style() -> void:
	_card.add_theme_stylebox_override("panel", DshTokens.box(
		DshTokens.bg_layer1(),
		DshTokens.RADIUS_LG,
		DshTokens.border_l2(),
		1,
		Vector4(18, 16, 18, 16)
	))
	_title.add_theme_color_override("font_color", DshTokens.text_primary())
	_empty.add_theme_color_override("font_color", DshTokens.text_tertiary())
	_output.add_theme_stylebox_override("normal", DshTokens.box(
		DshTokens.bg_code(),
		DshTokens.RADIUS_MD,
		DshTokens.border_l1(),
		1,
		Vector4(8, 8, 8, 8)
	))
	_output.add_theme_color_override("font_color", DshTokens.text_secondary())


func _apply_strings(_loc: String = "") -> void:
	_title.text = DshI18n.t("jobs.title")
	_refresh_btn.text = DshI18n.t("jobs.refresh")
	_kill_btn.text = DshI18n.t("jobs.kill")
	_empty.text = DshI18n.t("jobs.empty")
	if _output_lbl != null:
		_output_lbl.text = DshI18n.t("jobs.output")
		_output_lbl.add_theme_color_override("font_color", DshTokens.text_secondary())


func _refresh() -> void:
	if _client == null or _session_id == "":
		_render_jobs([])
		return
	_client.list_jobs(_session_id, Callable())


func _on_jobs_refreshed(jobs: Array) -> void:
	if not visible:
		return
	_render_jobs(jobs)


func _render_jobs(jobs: Array) -> void:
	_jobs = jobs
	_list.clear()
	_empty.visible = jobs.is_empty()
	for j in jobs:
		if not (j is Dictionary):
			continue
		var d: Dictionary = j
		var id := str(d.get("id", ""))
		var label := str(d.get("label", ""))
		var kind := str(d.get("kind", ""))
		var status := str(d.get("status", ""))
		var text := kind + " · " + status
		if label != "":
			text = label + " — " + text
		_list.add_item(text)
		_list.set_item_metadata(_list.item_count - 1, {"id": id})
		if id == _selected_id:
			_list.select(_list.item_count - 1)
	if _selected_id != "":
		_load_output(_selected_id)


func _on_item_selected(index: int) -> void:
	var meta: Variant = _list.get_item_metadata(index)
	if meta is Dictionary:
		_selected_id = str((meta as Dictionary).get("id", ""))
		_load_output(_selected_id)


func _load_output(job_id: String) -> void:
	if _client == null or _session_id == "" or job_id == "":
		_output.text = DshI18n.t("jobs.noOutput")
		return
	_client.read_job_output(_session_id, job_id, _on_output)


func _on_output(ok: bool, data: Variant) -> void:
	if ok and data is Dictionary:
		var text := str((data as Dictionary).get("output", ""))
		_output.text = text if text != "" else DshI18n.t("jobs.noOutput")
	else:
		_output.text = DshI18n.t("jobs.noOutput")


func _kill_selected() -> void:
	if _client == null or _session_id == "" or _selected_id == "":
		return
	# 内联二次确认：第一击武装（按钮转危险文案），3 秒内第二击才真正 kill。
	if not _kill_armed:
		_kill_armed = true
		# t() 对缺失键返回键名本身：非 "jobs.killConfirm" 即命中词表。
		var confirm_txt := DshI18n.t("jobs.killConfirm")
		_kill_btn.text = confirm_txt if confirm_txt != "jobs.killConfirm" else "Confirm kill?"
		_kill_btn.modulate = Color(1.0, 0.55, 0.55)
		_kill_arm_timer.start()
		return
	_disarm_kill()
	_client.kill_job(_session_id, _selected_id, _on_killed)


func _disarm_kill() -> void:
	_kill_armed = false
	_kill_arm_timer.stop()
	if _kill_btn:
		_kill_btn.text = DshI18n.t("jobs.kill")
		_kill_btn.modulate = Color.WHITE


func _on_killed(_ok: bool, _data: Variant) -> void:
	_refresh()


func _on_backdrop(event: InputEvent) -> void:
	if event is InputEventMouseButton:
		var mb := event as InputEventMouseButton
		if mb.pressed and mb.button_index == MOUSE_BUTTON_LEFT:
			close()


func _unhandled_input(event: InputEvent) -> void:
	if not visible:
		return
	if event is InputEventKey:
		var k := event as InputEventKey
		if k.pressed and not k.echo and k.keycode == KEY_ESCAPE:
			close()
			get_viewport().set_input_as_handled()
