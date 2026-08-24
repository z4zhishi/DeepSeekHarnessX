extends PanelContainer
class_name PlanCard

@onready var header_label: Label = %Header
@onready var body_label: RichTextLabel = %Body
@onready var icon_rect: TextureRect = %Icon

var _data: Dictionary = {}


func _ready() -> void:
	_apply_style()
	icon_rect.texture = load("res://assets/icons/icon_plan.svg") as Texture2D
	_refresh()


func _notification(what: int) -> void:
	if what == NOTIFICATION_THEME_CHANGED:
		_apply_style()
		_refresh()


func bind(node: Dictionary) -> void:
	setup(node.get("payload", {}) if node.get("payload") is Dictionary else node)


func setup(data: Dictionary) -> void:
	_data = data
	if is_node_ready():
		_refresh()


func _apply_style() -> void:
	add_theme_stylebox_override("panel", DshTokens.box(
		DshTokens.bg_layer2(),
		DshTokens.RADIUS_MD,
		DshTokens.accent(),
		1,
		Vector4(12, 8, 12, 8)
	))
	header_label.add_theme_font_size_override("font_size", DshTokens.FONT_CHROME)
	body_label.bbcode_enabled = true
	body_label.fit_content = true
	body_label.scroll_active = false
	body_label.autowrap_mode = TextServer.AUTOWRAP_WORD_SMART
	body_label.add_theme_font_size_override("normal_font_size", DshTokens.FONT_CAPTION)


func _refresh() -> void:
	var active := bool(_data.get("active", false))
	if icon_rect:
		icon_rect.modulate = DshTokens.success() if active else DshTokens.text_tertiary()
	if active:
		header_label.text = _t("chat.planActive", "Plan mode")
		header_label.add_theme_color_override("font_color", DshTokens.success())
		body_label.text = "[color=%s]%s[/color]" % [
			DshMarkdown.hex(DshTokens.text_secondary()),
			_t("chat.planBodyActive", "The assistant will outline steps and wait for review before edits."),
		]
	else:
		header_label.text = _t("chat.planInactive", "Plan mode off")
		header_label.add_theme_color_override("font_color", DshTokens.text_secondary())
		body_label.text = "[color=%s]%s[/color]" % [
			DshMarkdown.hex(DshTokens.text_tertiary()),
			_t("chat.planBodyInactive", "Run /plan on to draft a structured plan."),
		]


func _t(key: String, fallback: String) -> String:
	var s := DshI18n.t(key)
	return fallback if s == key else s
