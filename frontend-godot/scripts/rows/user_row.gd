extends MarginContainer
class_name UserRow

@onready var bubble: PanelContainer = %Bubble
@onready var body: RichTextLabel = %Body
@onready var chips: HBoxContainer = %Chips

var _plain: String = ""


func _ready() -> void:
	add_theme_constant_override("margin_left", 8)
	add_theme_constant_override("margin_right", 8)
	add_theme_constant_override("margin_top", 2)
	add_theme_constant_override("margin_bottom", 2)
	body.bbcode_enabled = true
	body.fit_content = true
	body.scroll_active = false
	body.autowrap_mode = TextServer.AUTOWRAP_WORD_SMART
	body.selection_enabled = true
	body.add_theme_font_size_override("normal_font_size", DshTokens.FONT_BODY)
	_apply_style()
	resized.connect(_fit_bubble)


func _notification(what: int) -> void:
	if what == NOTIFICATION_THEME_CHANGED:
		if bubble == null or body == null:
			return
		_apply_style()


func bind(node: Dictionary) -> void:
	if body == null:
		return
	var p: Dictionary = node.get("payload", {}) if node.get("payload") is Dictionary else {}
	_plain = str(p.get("text", ""))
	body.text = DshMarkdown.escape(_plain)
	body.add_theme_color_override("default_color", DshTokens.text_primary())
	_fill_chips(p.get("attachments", []))
	_fit_bubble()


func _apply_style() -> void:
	if bubble == null or body == null:
		return
	# 用户气泡：accent 描边 + 轻阴影（与 assistant 无框流形成对比）。
	var bubble_box := DshTokens.box(
		DshTokens.bg_bubble(),
		DshTokens.RADIUS_LG,
		DshTokens.border_l2(),
		1,
		Vector4(14, 10, 14, 10)
	)
	bubble_box.shadow_color = DshTokens.shadow_tinted()
	bubble_box.shadow_size = 10
	bubble_box.shadow_offset = Vector2(0, 4)
	bubble.add_theme_stylebox_override("panel", bubble_box)


func _fit_bubble() -> void:
	if bubble == null or body == null or size.x < 8.0:
		return
	var max_w := size.x * 0.82
	var font := body.get_theme_font("normal_font")
	var fs := body.get_theme_font_size("normal_font_size")
	if font == null:
		font = get_theme_default_font()
		fs = DshTokens.FONT_BODY
	var tw := font.get_multiline_string_size(_plain, HORIZONTAL_ALIGNMENT_LEFT, -1, fs).x + 36.0
	var w := clampf(tw, 56.0, max_w)
	bubble.custom_minimum_size.x = w
	body.custom_minimum_size.x = maxf(w - 28.0, 24.0)


func _fill_chips(attachments: Variant) -> void:
	if chips == null:
		return
	for c in chips.get_children():
		c.queue_free()
	if not (attachments is Array) or (attachments as Array).is_empty():
		chips.visible = false
		return
	chips.visible = true
	for a in attachments:
		var path := ""
		var name := ""
		if a is String:
			path = a
			name = String(a).get_file()
		elif a is Dictionary:
			path = str(a.get("path", ""))
			name = str(a.get("name", path.get_file()))
		if name == "":
			name = path if path != "" else "file"
		var chip := Label.new()
		chip.text = name
		chip.tooltip_text = path
		chip.add_theme_font_size_override("font_size", DshTokens.FONT_MICRO)
		chip.add_theme_color_override("font_color", DshTokens.accent())
		var wrap := PanelContainer.new()
		wrap.add_theme_stylebox_override("panel", DshTokens.box(
			DshTokens.bg_layer3(),
			DshTokens.RADIUS_SM,
			DshTokens.border_l2(),
			1,
			Vector4(8, 3, 8, 3)
		))
		var row := HBoxContainer.new()
		row.add_theme_constant_override("separation", 4)
		var ic := TextureRect.new()
		ic.texture = load("res://assets/icons/icon_paperclip.svg") as Texture2D
		ic.custom_minimum_size = Vector2(12, 12)
		ic.expand_mode = TextureRect.EXPAND_IGNORE_SIZE
		ic.stretch_mode = TextureRect.STRETCH_KEEP_ASPECT_CENTERED
		ic.modulate = DshTokens.accent()
		row.add_child(ic)
		row.add_child(chip)
		wrap.add_child(row)
		chips.add_child(wrap)
