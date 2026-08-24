extends MarginContainer
class_name SystemRow

@onready var caption: Label = %Caption


func _notification(what: int) -> void:
	if what == NOTIFICATION_THEME_CHANGED:
		if caption == null:
			return
		_apply_style()


func _ready() -> void:
	add_theme_constant_override("margin_left", 8)
	add_theme_constant_override("margin_right", 8)
	add_theme_constant_override("margin_top", 2)
	add_theme_constant_override("margin_bottom", 2)
	_apply_style()


func _apply_style() -> void:
	if caption == null:
		return
	caption.horizontal_alignment = HORIZONTAL_ALIGNMENT_CENTER
	caption.autowrap_mode = TextServer.AUTOWRAP_WORD_SMART
	caption.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)


func bind(node: Dictionary) -> void:
	if caption == null:
		return
	var kind := str(node.get("kind", "system"))
	var p: Dictionary = node.get("payload", {}) if node.get("payload") is Dictionary else {}
	var text := str(p.get("text", ""))
	if kind == "command":
		var name := str(p.get("name", ""))
		var args := str(p.get("args", ""))
		var status := str(p.get("status", ""))
		var line := "/" + name
		if args != "":
			line += " " + args
		if status != "" and status != "running":
			line += "  ·  " + status
		if text != "":
			line += "  —  " + text
		caption.text = line
		caption.add_theme_color_override("font_color", DshTokens.text_tertiary())
		return
	if text == "":
		text = str(p.get("reason", ""))
	if text == "":
		text = _t("chat.systemStopped", "Stopped.")
	caption.text = text
	if kind == "turn-error" or str(p.get("reason", "")) == "error":
		caption.add_theme_color_override("font_color", DshTokens.danger())
	else:
		caption.add_theme_color_override("font_color", DshTokens.text_muted())


func _t(key: String, fallback: String) -> String:
	var s := DshI18n.t(key)
	return fallback if s == key else s
