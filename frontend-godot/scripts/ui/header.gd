extends PanelContainer
class_name HeaderBar

signal tab_changed(name: String)
signal jobs_pressed
signal model_selected(id: String)

const HEADER_COMPACT := 1180.0

@onready var _title: Label = %Title
@onready var _lineage: Label = %Lineage
@onready var _chat_btn: Button = %ChatTabBtn
@onready var _traj_btn: Button = %TrajectoryTabBtn
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

func _ready() -> void:
	var group := ButtonGroup.new()
	_chat_btn.toggle_mode = true
	_traj_btn.toggle_mode = true
	_chat_btn.button_group = group
	_traj_btn.button_group = group
	_chat_btn.button_pressed = true
	_chat_btn.pressed.connect(func(): _emit_tab("chat"))
	_traj_btn.pressed.connect(func(): _emit_tab("trajectory"))
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
	_plan.add_theme_stylebox_override("panel", DshTokens.box(DshTokens.bg_layer2(), DshTokens.RADIUS_PILL, DshTokens.success(), 1, Vector4(8, 2, 8, 2)))
	_plan_label.add_theme_color_override("font_color", DshTokens.success())
	_plan_label.add_theme_font_size_override("font_size", DshTokens.FONT_MICRO)
	_apply_strings()
	_apply_compact()


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


func _refresh_model_visibility() -> void:
	# §5: the Header "model" surface shows whenever the model list is non-empty;
	# it only hides when the bar goes compact.
	_models.visible = _models.item_count > 0 and not is_compact()


func set_context(pressure: float, label: String) -> void:
	_ctx_bar.value = clampf(pressure, 0.0, 1.0)
	_ctx_label.text = label if label != "" else ("%d%%" % int(round(clampf(pressure, 0.0, 1.0) * 100.0)))


func set_plan_active(active: bool) -> void:
	_plan.visible = active


func _emit_tab(name: String) -> void:
	if _tab == name:
		return
	_tab = name
	_chat_btn.button_pressed = name == "chat"
	_traj_btn.button_pressed = name == "trajectory"
	tab_changed.emit(name)


func _on_model_item(index: int) -> void:
	if _syncing_models or index < 0:
		return
	var id := str(_models.get_item_metadata(index))
	if id != "":
		model_selected.emit(id)


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
	_plan_label.text = _t("app.planMode", "Plan")
	_jobs.text = _t("app.jobs", "Jobs")
	_models.tooltip_text = _t("common.model", "Model")
	if _title.text == "":
		set_title("")


func _t(key: String, fallback: String) -> String:
	var v := str(DshI18n.t(key))
	return fallback if v == key or v.strip_edges() == "" else v
