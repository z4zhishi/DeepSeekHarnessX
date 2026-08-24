extends CanvasLayer
class_name ApprovalOverlay

## Modal approval card. Decisions are backend strings: allow_once | deny | cancel.

signal decision_made(call_id: String, decision: String)

var _call_id: String = ""
var _backdrop: ColorRect
var _card: PanelContainer
var _title: Label
var _prompt: RichTextLabel
var _btn_row: HBoxContainer
var _icon: TextureRect


func _ready() -> void:
	layer = 24
	visible = false
	_build()
	if get_node_or_null("/root/DshI18n") != null:
		DshI18n.locale_changed.connect(_on_locale_changed)


func _build() -> void:
	_backdrop = ColorRect.new()
	_backdrop.color = Color(0, 0, 0, 0.55)
	_backdrop.set_anchors_preset(Control.PRESET_FULL_RECT)
	_backdrop.mouse_filter = Control.MOUSE_FILTER_STOP
	_backdrop.gui_input.connect(_on_backdrop_input)
	add_child(_backdrop)

	var center := CenterContainer.new()
	center.set_anchors_preset(Control.PRESET_FULL_RECT)
	center.mouse_filter = Control.MOUSE_FILTER_IGNORE
	add_child(center)

	_card = PanelContainer.new()
	_card.custom_minimum_size = Vector2(520, 260)
	center.add_child(_card)

	var box := VBoxContainer.new()
	box.add_theme_constant_override("separation", 14)
	_card.add_child(box)

	var title_row := HBoxContainer.new()
	title_row.add_theme_constant_override("separation", 8)
	box.add_child(title_row)

	_icon = TextureRect.new()
	_icon.custom_minimum_size = Vector2(20, 20)
	_icon.expand_mode = TextureRect.EXPAND_IGNORE_SIZE
	_icon.stretch_mode = TextureRect.STRETCH_KEEP_ASPECT_CENTERED
	var tex := load("res://assets/icons/icon_warning.svg")
	if tex is Texture2D:
		_icon.texture = tex
	_icon.modulate = DshTokens.warn()
	title_row.add_child(_icon)

	_title = Label.new()
	_title.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_title.add_theme_font_size_override("font_size", 16)
	title_row.add_child(_title)

	_prompt = RichTextLabel.new()
	_prompt.bbcode_enabled = true
	_prompt.fit_content = true
	_prompt.scroll_active = true
	_prompt.size_flags_vertical = Control.SIZE_EXPAND_FILL
	_prompt.custom_minimum_size = Vector2(0, 80)
	box.add_child(_prompt)

	_btn_row = HBoxContainer.new()
	_btn_row.add_theme_constant_override("separation", 10)
	_btn_row.alignment = BoxContainer.ALIGNMENT_END
	box.add_child(_btn_row)

	_apply_style()
	_title.text = DshI18n.t("approval.title")


func _apply_style() -> void:
	_card.add_theme_stylebox_override("panel", DshTokens.box(
		DshTokens.bg_layer2(),
		DshTokens.RADIUS_LG,
		DshTokens.warn(),
		1,
		Vector4(20, 18, 20, 18)
	))
	_title.add_theme_color_override("font_color", DshTokens.warn())
	_prompt.add_theme_color_override("default_color", DshTokens.text_primary())
	_icon.modulate = DshTokens.warn()


func _on_locale_changed(_loc: String) -> void:
	_title.text = DshI18n.t("approval.title")


func show_request(call_id: String, prompt: String, options: Array = []) -> void:
	_call_id = call_id
	_title.text = DshI18n.t("approval.title")
	_prompt.text = _esc_bb(prompt)
	_rebuild_buttons(options)
	_apply_style()
	visible = true


func hide_request() -> void:
	visible = false
	_call_id = ""


func _rebuild_buttons(options: Array) -> void:
	for c in _btn_row.get_children():
		_btn_row.remove_child(c)
		c.queue_free()
	var mapped: Array = _map_options(options)
	for item in mapped:
		if not (item is Dictionary):
			continue
		var d: Dictionary = item
		var decision := str(d.get("decision", "cancel"))
		var btn := Button.new()
		btn.custom_minimum_size = Vector2(110, 34)
		btn.text = str(d.get("label", decision))
		_style_decision_button(btn, decision)
		btn.pressed.connect(_decide.bind(decision))
		_btn_row.add_child(btn)


func _style_decision_button(btn: Button, decision: String) -> void:
	var bg: Color = DshTokens.bg_layer3()
	var hover: Color = DshTokens.border_l4()
	if decision == "allow_once":
		bg = DshTokens.brand_button()
		hover = DshTokens.text_secondary()
		btn.add_theme_color_override("font_color", DshTokens.bg_base())
		btn.add_theme_color_override("font_hover_color", DshTokens.bg_base())
	elif decision == "deny":
		bg = DshTokens.danger()
		hover = DshTokens.danger()
		btn.add_theme_color_override("font_color", Color(1, 1, 1, 1))
		btn.add_theme_color_override("font_hover_color", Color(1, 1, 1, 1))
	else:
		btn.add_theme_color_override("font_color", DshTokens.text_primary())
	btn.add_theme_stylebox_override("normal", DshTokens.box(bg, DshTokens.RADIUS_MD, Color(0, 0, 0, 0), 0, Vector4(12, 6, 12, 6)))
	btn.add_theme_stylebox_override("hover", DshTokens.box(hover, DshTokens.RADIUS_MD, Color(0, 0, 0, 0), 0, Vector4(12, 6, 12, 6)))
	btn.add_theme_stylebox_override("pressed", DshTokens.box(hover, DshTokens.RADIUS_MD, Color(0, 0, 0, 0), 0, Vector4(12, 6, 12, 6)))


func _map_options(options: Array) -> Array:
	var out: Array = []
	if options.is_empty():
		return [
			{"decision": "cancel", "label": DshI18n.t("approval.cancel")},
			{"decision": "deny", "label": DshI18n.t("approval.reject")},
			{"decision": "allow_once", "label": DshI18n.t("approval.allow")},
		]
	for raw in options:
		var id := ""
		var label := ""
		if raw is Dictionary:
			var d: Dictionary = raw
			id = str(d.get("optionId", d.get("id", d.get("value", d.get("decision", "")))))
			label = str(d.get("name", d.get("label", d.get("title", ""))))
		else:
			id = str(raw)
			label = str(raw)
		var decision := _normalize_decision(id if id != "" else label)
		if label == "":
			label = _label_for(decision)
		out.append({"decision": decision, "label": label})
	if out.is_empty():
		return _map_options([])
	return out


func _normalize_decision(raw: String) -> String:
	var s := raw.strip_edges()
	var lower := s.to_lower().replace(" ", "_")
	if lower == "allow_once" or lower == "allow-once" or lower == "allowonce":
		return "allow_once"
	if lower == "allow" or lower == "y" or lower == "yes" or lower == "a":
		return "allow_once"
	if s.to_lower() == "allow once":
		return "allow_once"
	if lower == "deny" or lower == "reject" or lower == "n" or lower == "no" or lower == "d":
		return "deny"
	if lower == "cancel" or lower == "c":
		return "cancel"
	return "cancel" if s == "" else s


func _label_for(decision: String) -> String:
	if decision == "allow_once":
		return DshI18n.t("approval.allow")
	if decision == "deny":
		return DshI18n.t("approval.reject")
	if decision == "cancel":
		return DshI18n.t("approval.cancel")
	return decision


func _decide(decision: String) -> void:
	if _call_id == "":
		return
	var cid := _call_id
	_call_id = ""
	visible = false
	decision_made.emit(cid, decision)


func _on_backdrop_input(event: InputEvent) -> void:
	if event is InputEventMouseButton:
		var mb := event as InputEventMouseButton
		if mb.pressed and mb.button_index == MOUSE_BUTTON_LEFT:
			_decide("cancel")


func _unhandled_input(event: InputEvent) -> void:
	if not visible:
		return
	if event is InputEventKey:
		var k := event as InputEventKey
		if k.pressed and not k.echo and k.keycode == KEY_ESCAPE:
			_decide("cancel")
			get_viewport().set_input_as_handled()


func _esc_bb(s: String) -> String:
	return s.replace("[", "[lb]").replace("]", "[/lb]")
