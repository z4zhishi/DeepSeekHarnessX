extends PanelContainer
class_name HeaderBar

signal tab_changed(name: String)
signal jobs_pressed
signal model_selected(id: String)
signal param_effort_changed(effort: String)

const HEADER_COMPACT := 1180.0

@onready var _title: Label = %Title
@onready var _lineage: Label = %Lineage
@onready var _chat_btn: Button = %ChatTabBtn
@onready var _traj_btn: Button = %TrajectoryTabBtn
@onready var _lineage_btn: Button = %LineageTabBtn
@onready var _models: OptionButton = %ModelPicker
@onready var _ctx: HBoxContainer = %CtxPressure
@onready var _ctx_bar: ProgressBar = %CtxBar
@onready var _ctx_label: Label = %CtxLabel
@onready var _plan: PanelContainer = %PlanBadge
@onready var _plan_icon: TextureRect = %PlanBadgeIcon
@onready var _plan_label: Label = %PlanBadgeLabel
@onready var _jobs: Button = %JobsBtn
@onready var _jobs_icon: TextureRect = %JobsIcon

var _syncing_models := false
var _tab := "chat"
var _param_btn: MenuButton = null
var _param_popup: PopupMenu = null
var _effort_opt: OptionButton = null
var _effort := "high"

func _ready() -> void:
	var group := ButtonGroup.new()
	_chat_btn.toggle_mode = true
	_traj_btn.toggle_mode = true
	_lineage_btn.toggle_mode = true
	_chat_btn.button_group = group
	_traj_btn.button_group = group
	_lineage_btn.button_group = group
	_chat_btn.button_pressed = true
	_chat_btn.pressed.connect(func(): _emit_tab("chat"))
	_traj_btn.pressed.connect(func(): _emit_tab("trajectory"))
	_lineage_btn.pressed.connect(func(): _emit_tab("lineage"))
	_jobs.pressed.connect(func(): jobs_pressed.emit())
	_models.item_selected.connect(_on_model_item)
	resized.connect(_apply_compact)
	if DshI18n.has_signal("locale_changed"):
		DshI18n.locale_changed.connect(func(_loc: String): _apply_strings())
	_ctx_bar.min_value = 0.0
	_ctx_bar.max_value = 1.0
	_ctx_bar.show_percentage = false
	apply_tokens()
	_apply_strings()
	set_plan_active(false)
	_build_param_popup()
	call_deferred("_refresh_model_visibility")
	call_deferred("_apply_compact")


func apply_tokens() -> void:
	var sb := DshTokens.box(DshTokens.bg_base(), 0, DshTokens.border_l1(), 1, Vector4.ZERO)
	sb.border_width_left = 0
	sb.border_width_top = 0
	sb.border_width_right = 0
	add_theme_stylebox_override("panel", sb)
	_title.add_theme_color_override("font_color", DshTokens.text_primary())
	_title.add_theme_font_size_override("font_size", DshTokens.FONT_CHROME_LG)
	_lineage.add_theme_color_override("font_color", DshTokens.text_tertiary())
	_lineage.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
	_ctx_label.add_theme_color_override("font_color", DshTokens.text_tertiary())
	_ctx_label.add_theme_font_size_override("font_size", DshTokens.FONT_MICRO)
	DshIcons.apply(_plan_icon, "plan", 14.0)
	DshIcons.apply(_jobs_icon, "jobs", 14.0)
	# Jobs 按钮（Apple 化第一批，截图审出缺陷 2）：全局 Button 主题在暗色下
	# 会把无 override 的按钮画成反白实底块——Header 内不应有色块按钮。
	# 改为 quiet ghost：透明底 + hover 轻抬亮，与 tabs 同一视觉语言。
	var jpad := Vector4(12, 5, 12, 5)
	_jobs.flat = true
	_jobs.add_theme_stylebox_override("normal", DshTokens.box(Color(0, 0, 0, 0), DshTokens.RADIUS_PILL, Color(0, 0, 0, 0), 0, jpad))
	_jobs.add_theme_stylebox_override("hover", DshTokens.box(DshTokens.bg_layer2(), DshTokens.RADIUS_PILL, Color(0, 0, 0, 0), 0, jpad))
	_jobs.add_theme_stylebox_override("pressed", DshTokens.box(DshTokens.pressed_layer(), DshTokens.RADIUS_PILL, Color(0, 0, 0, 0), 0, jpad))
	_jobs.add_theme_stylebox_override("focus", DshTokens.box(Color(0, 0, 0, 0), DshTokens.RADIUS_PILL, DshTokens.accent(), 1, jpad))
	_jobs.add_theme_color_override("font_color", DshTokens.text_secondary())
	_jobs.add_theme_color_override("font_hover_color", DshTokens.text_primary())
	_jobs.mouse_default_cursor_shape = Control.CURSOR_POINTING_HAND
	var plan_box := DshTokens.box(DshTokens.bg_layer2(), DshTokens.RADIUS_PILL, DshTokens.success(), 1, Vector4(10, 3, 10, 3))
	_plan.add_theme_stylebox_override("panel", plan_box)
	_plan_label.add_theme_color_override("font_color", DshTokens.success())
	_plan_label.add_theme_font_size_override("font_size", DshTokens.FONT_MICRO)
	_apply_strings()
	_apply_compact()
	_paint_tabs()


func set_title(text: String) -> void:
	_title.text = text if text != "" else _t("app.activeConversation", "Active conversation")


func set_lineage(parts: PackedStringArray) -> void:
	if parts.is_empty():
		_lineage.text = ""
	else:
		_lineage.text = " › ".join(parts)
	_apply_compact()


func set_models(models: Array, selected: String) -> void:
	_syncing_models = true
	_models.clear()
	var pick := 0
	for m in models:
		var id := ""
		var label := ""
		if m is Dictionary:
			id = str(m.get("id", ""))
			label = str(m.get("name", id))
		else:
			id = str(m)
			label = id
		if id == "":
			continue
		_models.add_item(label)
		var idx := _models.item_count - 1
		_models.set_item_metadata(idx, id)
		if id == selected:
			pick = idx
	if _models.item_count > 0:
		_models.select(pick)
		_models.disabled = false
	else:
		_models.disabled = true
	_syncing_models = false
	_refresh_model_visibility()
	_refresh_param_popup()


func _refresh_model_visibility() -> void:
	# Composer owns the primary model control. Keep this picker hidden as a
	# data-sync surface for set_models / the ⚙ param popup.
	_models.visible = false
	if _models.item_count <= 0:
		_models.tooltip_text = _t("common.model", "Model")
		return
	var id := ""
	if _models.selected >= 0 and _models.selected < _models.item_count:
		id = str(_models.get_item_metadata(_models.selected))
	if id == "":
		id = _models.text
	_models.tooltip_text = id if id != "" else _t("common.model", "Model")
	if is_compact() and id != "":
		_models.text = _compact_model_text(id)


func set_context(pressure: float, label: String, detail: String = "") -> void:
	pressure = clampf(pressure, 0.0, 1.0)
	_ctx_bar.value = pressure
	_ctx_label.text = label if label != "" else ("%d%%" % int(round(pressure * 100.0)))
	_ctx_label.tooltip_text = detail
	var fill := DshTokens.accent() if pressure < 0.8 else (DshTokens.warn() if pressure < 0.95 else DshTokens.danger())
	var bg := DshTokens.bg_layer2()
	var fill_box := DshTokens.box(fill, DshTokens.RADIUS_PILL, Color.TRANSPARENT, 0, Vector4(0, 0, 0, 0))
	fill_box.shadow_color = fill
	fill_box.shadow_size = 0 if pressure < 0.7 else 6
	_ctx_bar.add_theme_stylebox_override("fill", fill_box)
	_ctx_bar.add_theme_stylebox_override("background", DshTokens.box(bg, DshTokens.RADIUS_PILL, DshTokens.border_l1(), 1, Vector4(0, 0, 0, 0)))


func set_plan_active(active: bool) -> void:
	_plan.visible = active


func _emit_tab(name: String) -> void:
	if _tab == name:
		return
	_tab = name
	_chat_btn.button_pressed = name == "chat"
	_traj_btn.button_pressed = name == "trajectory"
	_lineage_btn.button_pressed = name == "lineage"
	_paint_tabs()
	tab_changed.emit(name)


func _paint_tabs() -> void:
	# 视觉重设计（task #20）：活动态 accent 弱底 + accent 文字（不再是白字
	# 实底块），非活动态透明、hover 才抬亮；tab 从"色块"降为"分层"——更轻，
	# 与收件条/侧栏的行语言一致。
	var idle_fg := DshTokens.text_secondary()
	for pair in [[_chat_btn, _tab == "chat"], [_traj_btn, _tab == "trajectory"], [_lineage_btn, _tab == "lineage"]]:
		var btn: Button = pair[0]
		var active: bool = pair[1]
		if btn == null:
			continue
		var bg := DshTokens.accent_soft() if active else Color(0, 0, 0, 0)
		var hov := DshTokens.accent_soft() if active else DshTokens.bg_layer2()
		btn.add_theme_stylebox_override("normal", DshTokens.box(bg, DshTokens.RADIUS_PILL, Color.TRANSPARENT, 0, Vector4(14, 5, 14, 4)))
		btn.add_theme_stylebox_override("hover", DshTokens.box(hov, DshTokens.RADIUS_PILL, Color.TRANSPARENT, 0, Vector4(14, 5, 14, 4)))
		btn.add_theme_stylebox_override("pressed", DshTokens.box(DshTokens.pressed_layer(), DshTokens.RADIUS_PILL, Color.TRANSPARENT, 0, Vector4(14, 5, 14, 4)))
		btn.add_theme_stylebox_override("focus", DshTokens.box(Color(0, 0, 0, 0), DshTokens.RADIUS_PILL, DshTokens.accent(), 1, Vector4(14, 5, 14, 4)))
		var fg := DshTokens.accent() if active else idle_fg
		var fg_hover := DshTokens.accent_hover() if active else DshTokens.text_primary()
		btn.add_theme_color_override("font_color", fg)
		btn.add_theme_color_override("font_hover_color", fg_hover)
		btn.add_theme_color_override("font_pressed_color", fg)
		btn.add_theme_color_override("font_focus_color", fg)


## _build_param_popup adds the "⚙" collapse button that opens a single
## model/engine parameters overlay (effort + model + context). This is the
## "收纳进触手可及处，点开再弹出" principle: the header stays uncluttered, and
## secondary knobs live in one popup instead of a row of always-on controls.
func _build_param_popup() -> void:
	if _param_btn != null:
		return
	_param_btn = MenuButton.new()
	_param_btn.text = "⚙"
	_param_btn.tooltip_text = _t("app.params", "模型 / 工程参数")
	_param_btn.flat = true
	_param_btn.custom_minimum_size = Vector2(30, 0)
	# Place at the far right of the header HBox, after Jobs.
	var hbox := _jobs.get_parent() as HBoxContainer
	if hbox:
		hbox.add_child(_param_btn)
		hbox.move_child(_param_btn, hbox.get_child_count() - 1)
	_param_popup = _param_btn.get_popup()
	_param_popup.id_pressed.connect(_on_param_action)
	_refresh_param_popup()


## Refresh the parameter popup contents (effort modes + models).
## Called on locale change and after models load.
func _refresh_param_popup() -> void:
	if _param_popup == null:
		return
	_param_popup.clear()
	var efforts := [
		["high", _t("app.effortHigh", "Effort: high")],
		["low", _t("app.effortLow", "Effort: low")],
		["max", _t("app.effortMax", "Effort: max")],
		["off", _t("app.effortOff", "Effort: off")],
	]
	for i in efforts.size():
		_param_popup.add_radio_check_item(efforts[i][1], 100 + i)
		_param_popup.set_item_checked(i, efforts[i][0] == _effort)
	_param_popup.add_separator()
	# Model entries mirror the OptionButton so both surfaces stay in sync.
	if _models.item_count > 0:
		for i in _models.item_count:
			var id := str(_models.get_item_metadata(i))
			var lbl := _models.get_item_text(i)
			if id == "":
				continue
			_param_popup.add_radio_check_item(lbl, 1000 + i)
			_param_popup.set_item_metadata(_param_popup.item_count - 1, id)
		_param_popup.add_separator()


func _effort_idx() -> int:
	match _effort:
		"low":
			return 1
		"max":
			return 2
		"off":
			return 3
		_:
			return 0



func _on_param_action(id: int) -> void:
	if id >= 100 and id <= 103:
		var e: String = str(["high", "low", "max", "off"][id - 100])
		_effort = e
		param_effort_changed.emit(e)
		_refresh_param_popup()
	elif id >= 1000:
		var idx := id - 1000
		if idx >= 0 and idx < _models.item_count:
			var mid := str(_models.get_item_metadata(idx))
			if mid != "":
				model_selected.emit(mid)


## set_effort syncs the header's effort display from the backend (a resumed
## session's real value, never the default "high").
func set_effort(effort: String) -> void:
	if effort == "":
		return
	_effort = effort
	if _param_popup != null:
		_refresh_param_popup()


func _on_model_item(index: int) -> void:
	if _syncing_models or index < 0:
		return
	var id := str(_models.get_item_metadata(index))
	if id != "":
		model_selected.emit(id)


# compact 态的模型缩写：取 id 末段（deepseek-v4-flash → v4-flash）。
func _compact_model_text(id: String) -> String:
	if id == "":
		return id
	var parts := id.split("-")
	if parts.size() >= 2:
		return "-".join(parts.slice(parts.size() - 2))
	return id


func _apply_compact() -> void:
	var compact := get_viewport_rect().size.x < HEADER_COMPACT
	_ctx.visible = true
	_lineage.visible = (not compact) and _lineage.text.strip_edges() != ""
	_refresh_model_visibility()


func is_compact() -> bool:
	return get_viewport_rect().size.x < HEADER_COMPACT


func _apply_strings() -> void:
	_chat_btn.text = _t("app.chatTab", "Chat")
	_traj_btn.text = _t("app.trajectoryTab", "Trajectory")
	_lineage_btn.text = _t("app.lineageTab", "Lineage")
	_plan_label.text = _t("app.planMode", "Plan")
	_jobs.text = _t("app.jobs", "Jobs")
	_jobs.tooltip_text = "%s (Ctrl+J)" % _t("app.jobs", "Jobs")
	# _models tooltip is managed by _refresh_model_visibility (full id when compact).
	if _models.item_count == 0:
		_models.tooltip_text = _t("common.model", "Model")
	if _title.text == "":
		set_title("")
	_refresh_param_popup()


func _t(key: String, fallback: String) -> String:
	var v := str(DshI18n.t(key))
	return fallback if v == key or v.strip_edges() == "" else v
