extends MarginContainer
class_name ReasoningRow

signal height_changed

@onready var toggle_btn: Button = %Toggle
@onready var preview: Label = %Preview
@onready var body: RichTextLabel = %Body

var _text: String = ""
var _expanded: bool = false


func _notification(what: int) -> void:
	if what == NOTIFICATION_THEME_CHANGED:
		if toggle_btn == null or body == null:
			return
		_apply_theme()


func _ready() -> void:
	add_theme_constant_override("margin_left", 8)
	add_theme_constant_override("margin_right", 8)
	add_theme_constant_override("margin_top", 0)
	add_theme_constant_override("margin_bottom", 0)
	toggle_btn.flat = true
	toggle_btn.icon = load("res://assets/icons/icon_think.svg") as Texture2D
	toggle_btn.text = "Think"
	toggle_btn.alignment = HORIZONTAL_ALIGNMENT_LEFT
	toggle_btn.focus_mode = Control.FOCUS_NONE
	var empty := StyleBoxEmpty.new()
	toggle_btn.add_theme_stylebox_override("normal", empty)
	toggle_btn.add_theme_stylebox_override("hover", empty)
	toggle_btn.add_theme_stylebox_override("pressed", empty)
	if not toggle_btn.pressed.is_connected(_toggle):
		toggle_btn.pressed.connect(_toggle)
	preview.autowrap_mode = TextServer.AUTOWRAP_OFF
	preview.text_overrun_behavior = TextServer.OVERRUN_TRIM_ELLIPSIS
	body.bbcode_enabled = true
	body.fit_content = true
	body.scroll_active = false
	body.autowrap_mode = TextServer.AUTOWRAP_WORD_SMART
	body.selection_enabled = true
	body.visible = false
	_apply_theme()


func bind(node: Dictionary) -> void:
	var p: Dictionary = node.get("payload", {}) if node.get("payload") is Dictionary else {}
	_text = str(p.get("text", ""))
	_expanded = bool(p.get("expanded", false))
	_apply_state()


func set_stream_text(text: String) -> void:
	_text = text
	_apply_state()


func is_expanded() -> bool:
	return _expanded


func _toggle() -> void:
	_expanded = not _expanded
	_apply_state()
	height_changed.emit()


func _apply_theme() -> void:
	if toggle_btn == null or body == null:
		return
	toggle_btn.add_theme_color_override("font_color", DshTokens.text_tertiary())
	toggle_btn.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
	if preview != null:
		preview.add_theme_color_override("font_color", DshTokens.text_muted())
		preview.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
	body.add_theme_font_size_override("normal_font_size", DshTokens.FONT_CAPTION)
	body.add_theme_color_override("default_color", DshTokens.text_tertiary())
	_apply_state()


func _apply_state() -> void:
	if toggle_btn == null or body == null:
		return
	body.visible = _expanded
	if preview != null:
		preview.visible = not _expanded
		preview.text = _preview_line(_text)
	if _expanded:
		var dim := DshMarkdown.hex(DshTokens.text_tertiary())
		body.text = "[color=%s]%s[/color]" % [dim, DshMarkdown.escape(_text)]
	toggle_btn.modulate = DshTokens.text_secondary() if _expanded else DshTokens.text_tertiary()


func _preview_line(text: String) -> String:
	var t := text.strip_edges()
	if t == "":
		return ""
	var lines := t.split("\n")
	var first := str(lines[0]).strip_edges()
	var last := str(lines[lines.size() - 1]).strip_edges()
	if lines.size() == 1 or first == last:
		return first
	return "%s … %s" % [first, last]
