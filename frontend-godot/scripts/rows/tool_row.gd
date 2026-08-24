extends MarginContainer
class_name ToolRow

signal height_changed
signal tool_selected(call_id: String, name: String, input: String, output: String)

const SCENE_DIFF := preload("res://scenes/cards/diff_block.tscn")
const SCENE_TERM := preload("res://scenes/cards/terminal_block.tscn")

@onready var head: HBoxContainer = %Head
@onready var icon_rect: TextureRect = %Icon
@onready var name_label: Label = %Name
@onready var status_label: Label = %Status
@onready var expand: VBoxContainer = %Expand
@onready var in_body: RichTextLabel = %InBody
@onready var out_host: VBoxContainer = %OutHost
@onready var inspect_btn: Button = %InspectBtn

var _call_id: String = ""
var _name: String = ""
var _arguments: String = ""
var _output: String = ""
var _view: Dictionary = {}
var _status: String = "running"
var _expanded: bool = false


func _notification(what: int) -> void:
	if what == NOTIFICATION_THEME_CHANGED:
		if head == null or name_label == null:
			return
		_apply_style()


func _ready() -> void:
	add_theme_constant_override("margin_left", 8)
	add_theme_constant_override("margin_right", 8)
	add_theme_constant_override("margin_top", 0)
	add_theme_constant_override("margin_bottom", 0)
	head.mouse_filter = Control.MOUSE_FILTER_STOP
	head.mouse_default_cursor_shape = Control.CURSOR_POINTING_HAND
	if not head.gui_input.is_connected(_on_head_input):
		head.gui_input.connect(_on_head_input)
	if status_label != null:
		status_label.add_theme_font_size_override("font_size", DshTokens.FONT_MICRO)
	icon_rect.custom_minimum_size = Vector2(16, 16)
	icon_rect.expand_mode = TextureRect.EXPAND_IGNORE_SIZE
	icon_rect.stretch_mode = TextureRect.STRETCH_KEEP_ASPECT_CENTERED
	in_body.bbcode_enabled = true
	in_body.fit_content = true
	in_body.scroll_active = false
	in_body.autowrap_mode = TextServer.AUTOWRAP_WORD_SMART
	in_body.selection_enabled = true
	in_body.add_theme_font_override("mono_font", DshThemeBuilder.code_font())
	in_body.add_theme_font_size_override("normal_font_size", DshTokens.FONT_CODE)
	inspect_btn.flat = true
	inspect_btn.text = _t("details.toolInspector", "Inspect")
	inspect_btn.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
	if not inspect_btn.pressed.is_connected(_inspect):
		inspect_btn.pressed.connect(_inspect)
	var in_lab := expand.get_node_or_null("InLabel") as Label
	var out_lab := expand.get_node_or_null("OutLabel") as Label
	if in_lab:
		in_lab.add_theme_font_size_override("font_size", DshTokens.FONT_MICRO)
	if out_lab:
		out_lab.add_theme_font_size_override("font_size", DshTokens.FONT_MICRO)
	expand.visible = false
	_apply_style()


func _apply_style() -> void:
	if head == null or name_label == null:
		return
	name_label.add_theme_font_size_override("font_size", DshTokens.FONT_CHROME)
	name_label.add_theme_color_override("font_color", DshTokens.text_primary())
	if inspect_btn != null:
		inspect_btn.add_theme_color_override("font_color", DshTokens.accent())
	if expand != null:
		var in_lab := expand.get_node_or_null("InLabel") as Label
		var out_lab := expand.get_node_or_null("OutLabel") as Label
		if in_lab:
			in_lab.add_theme_color_override("font_color", DshTokens.text_tertiary())
		if out_lab:
			out_lab.add_theme_color_override("font_color", DshTokens.text_tertiary())
	_paint_status()
	_paint_icon()


func bind(node: Dictionary) -> void:
	if head == null or name_label == null:
		return
	var p: Dictionary = node.get("payload", {}) if node.get("payload") is Dictionary else {}
	_call_id = str(p.get("callId", ""))
	_name = str(p.get("name", ""))
	_arguments = str(p.get("arguments", ""))
	_output = str(p.get("output", ""))
	_view = p.get("view", {}) if p.get("view") is Dictionary else {}
	_status = str(p.get("status", "running"))
	_expanded = bool(p.get("expanded", false))
	name_label.text = _name if _name != "" else "tool"
	_paint_status()
	_paint_icon()
	_apply_expand()


func is_expanded() -> bool:
	return _expanded


func _on_head_input(event: InputEvent) -> void:
	if event is InputEventMouseButton:
		var mb := event as InputEventMouseButton
		if mb.pressed and mb.button_index == MOUSE_BUTTON_LEFT:
			_toggle()


func _toggle() -> void:
	_expanded = not _expanded
	_apply_expand()
	height_changed.emit()


func _apply_expand() -> void:
	if expand == null:
		return
	expand.visible = _expanded
	if _expanded:
		_fill_expand()
		return
	if out_host == null:
		return
	for c in out_host.get_children():
		out_host.remove_child(c)
		c.queue_free()


func _inspect() -> void:
	tool_selected.emit(_call_id, _name, _arguments, _output if _output != "" else _view_text())


func _paint_status() -> void:
	if status_label == null:
		return
	match _status:
		"running":
			status_label.text = _t("chat.running", "running")
			status_label.add_theme_color_override("font_color", DshTokens.accent())
		"error":
			status_label.text = _t("chat.error", "error")
			status_label.add_theme_color_override("font_color", DshTokens.danger())
		_:
			status_label.text = _t("chat.done", "done")
			status_label.add_theme_color_override("font_color", DshTokens.success())


func _paint_icon() -> void:
	if icon_rect == null:
		return
	var kind := str(_view.get("kind", ""))
	var path := "res://assets/icons/icon_terminal.svg"
	if kind == "diff":
		path = "res://assets/icons/icon_diff.svg"
	elif _name.find("bash") >= 0 or _name.find("shell") >= 0 or _name == "exec":
		path = "res://assets/icons/icon_terminal.svg"
	icon_rect.texture = load(path) as Texture2D
	match _status:
		"error":
			icon_rect.modulate = DshTokens.danger()
		"running":
			icon_rect.modulate = DshTokens.accent()
		_:
			icon_rect.modulate = DshTokens.text_secondary()


func _fill_expand() -> void:
	if in_body == null or out_host == null:
		return
	var pretty := _pretty(_arguments)
	in_body.text = "[code]%s[/code]" % DshMarkdown.escape(pretty)
	in_body.add_theme_color_override("default_color", DshTokens.text_secondary())
	for c in out_host.get_children():
		out_host.remove_child(c)
		c.queue_free()
	var kind := str(_view.get("kind", ""))
	if kind == "diff":
		var card: DiffBlock = SCENE_DIFF.instantiate()
		out_host.add_child(card)
		card.setup_from_view(_view)
	elif kind == "terminal":
		var card: TerminalBlock = SCENE_TERM.instantiate()
		out_host.add_child(card)
		card.setup_from_view(_view)
	else:
		var rtl := RichTextLabel.new()
		rtl.bbcode_enabled = true
		rtl.fit_content = true
		rtl.scroll_active = false
		rtl.autowrap_mode = TextServer.AUTOWRAP_WORD_SMART
		rtl.selection_enabled = true
		rtl.add_theme_font_override("mono_font", DshThemeBuilder.code_font())
		rtl.add_theme_font_size_override("normal_font_size", DshTokens.FONT_CODE)
		var out := _output if _output != "" else str(_view.get("text", ""))
		rtl.text = "[code]%s[/code]" % DshMarkdown.escape(out)
		rtl.add_theme_color_override("default_color", DshTokens.text_secondary())
		out_host.add_child(rtl)


func _pretty(json_str: String) -> String:
	if json_str == "":
		return ""
	var parsed: Variant = JSON.parse_string(json_str)
	if parsed == null:
		return json_str
	return JSON.stringify(parsed, "  ", false)


func _view_text() -> String:
	var kind := str(_view.get("kind", ""))
	if kind == "terminal":
		var term: Dictionary = _view.get("terminal", {}) if _view.get("terminal") is Dictionary else {}
		var lines: Variant = term.get("lines", [])
		if lines is Array:
			var ps := PackedStringArray()
			for ln in lines:
				ps.append(str(ln))
			return "\n".join(ps)
	if kind == "diff":
		return str(_view.get("text", ""))
	return str(_view.get("text", _output))


func _t(key: String, fallback: String) -> String:
	var s := DshI18n.t(key)
	return fallback if s == key else s
