extends PanelContainer
class_name GoalCard

@onready var header_label: Label = %Header
@onready var body_label: Label = %Body
@onready var phase_label: Label = %Phase
@onready var icon_rect: TextureRect = %Icon

var _data: Dictionary = {}
var _applying_style: bool = false


func _ready() -> void:
	_apply_style()
	icon_rect.texture = load("res://assets/icons/icon_goal.svg") as Texture2D
	icon_rect.modulate = DshTokens.accent()
	_refresh()


func _notification(what: int) -> void:
	if what == NOTIFICATION_THEME_CHANGED and not _applying_style:
		_apply_style()
		_refresh()


func bind(node: Dictionary) -> void:
	setup(node.get("payload", {}) if node.get("payload") is Dictionary else node)


func setup(data: Dictionary) -> void:
	_data = data
	if is_node_ready():
		_refresh()


func _apply_style() -> void:
	if _applying_style:
		return
	_applying_style = true
	add_theme_stylebox_override("panel", DshTokens.box(
		DshTokens.bg_layer2(),
		DshTokens.RADIUS_MD,
		DshTokens.border_l2(),
		1,
		Vector4(12, 8, 12, 8)
	))
	if header_label != null:
		header_label.add_theme_font_size_override("font_size", DshTokens.FONT_CHROME)
		header_label.add_theme_color_override("font_color", DshTokens.text_primary())
	if body_label != null:
		body_label.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
		body_label.autowrap_mode = TextServer.AUTOWRAP_WORD_SMART
	if phase_label != null:
		phase_label.add_theme_font_size_override("font_size", DshTokens.FONT_MICRO)
	_applying_style = false


func _refresh() -> void:
	if header_label == null or body_label == null or phase_label == null:
		return
	var op := str(_data.get("operation", ""))
	var goal: Dictionary = _data.get("goal", {}) if _data.get("goal") is Dictionary else {}
	var objective := str(goal.get("objective", ""))
	var phase := str(goal.get("phase", ""))
	var header := _t("chat.agentGoal", "Goal")
	if op != "":
		header += " · " + op
	header_label.text = header
	if op == "clear":
		var cleared: Dictionary = _data.get("cleared", {}) if _data.get("cleared") is Dictionary else {}
		body_label.text = _t("chat.goalCleared", "Cleared") + "  " + str(cleared.get("id", ""))
		body_label.add_theme_color_override("font_color", DshTokens.text_tertiary())
		phase_label.text = ""
		return
	if objective == "":
		body_label.text = "—"
	else:
		body_label.text = objective
	body_label.add_theme_color_override("font_color", DshTokens.text_secondary())
	var rounds := int(_data.get("roundsStarted", 0))
	var maxr := int(goal.get("maxGoalRounds", 0))
	var bits: PackedStringArray = PackedStringArray()
	if phase != "":
		bits.append(_phase_text(phase))
	if maxr > 0:
		bits.append("%d/%d" % [rounds, maxr])
	elif rounds > 0:
		bits.append("%d" % rounds)
	phase_label.text = " · ".join(bits)
	phase_label.add_theme_color_override("font_color", _phase_color(phase))


func _phase_text(phase: String) -> String:
	match phase:
		"active":
			return "active"
		"paused":
			return "paused"
		"blocked":
			return "blocked"
		"complete":
			return "complete"
		_:
			return phase


func _phase_color(phase: String) -> Color:
	match phase:
		"active":
			return DshTokens.success()
		"paused":
			return DshTokens.warn()
		"blocked":
			return DshTokens.danger()
		"complete":
			return DshTokens.accent()
		_:
			return DshTokens.text_tertiary()


func _t(key: String, fallback: String) -> String:
	var s := DshI18n.t(key)
	return fallback if s == key else s
