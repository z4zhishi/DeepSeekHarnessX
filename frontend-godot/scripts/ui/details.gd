extends PanelContainer
class_name DetailsPane

signal close_requested

@onready var _title: Label = %DetailsTitle
@onready var _close: Button = %CloseBtn
@onready var _close_icon: TextureRect = %CloseIcon
@onready var _empty: Label = %EmptyLabel
@onready var _body: ScrollContainer = %Body
@onready var _in_label: Label = %InLabel
@onready var _in_text: TextEdit = %InText
@onready var _out_label: Label = %OutLabel
@onready var _out_text: TextEdit = %OutText

func _ready() -> void:
	clip_contents = true
	_close.pressed.connect(func(): close_requested.emit())
	_in_text.editable = false
	_out_text.editable = false
	_in_text.wrap_mode = TextEdit.LINE_WRAPPING_BOUNDARY
	_out_text.wrap_mode = TextEdit.LINE_WRAPPING_BOUNDARY
	_in_text.scroll_fit_content_height = true
	_out_text.scroll_fit_content_height = true
	if DshI18n.has_signal("locale_changed"):
		DshI18n.locale_changed.connect(func(_loc: String): _apply_strings())
	apply_tokens()
	_apply_strings()
	_show_empty(true)


func apply_tokens() -> void:
	var sb := DshTokens.box(DshTokens.bg_sidebar(), 0, DshTokens.border_l2(), 1, Vector4.ZERO)
	sb.border_width_right = 0
	sb.border_width_top = 0
	sb.border_width_bottom = 0
	add_theme_stylebox_override("panel", sb)
	_title.add_theme_color_override("font_color", DshTokens.text_primary())
	_title.add_theme_font_size_override("font_size", DshTokens.FONT_UI)
	_empty.add_theme_color_override("font_color", DshTokens.text_tertiary())
	_empty.add_theme_font_size_override("font_size", DshTokens.FONT_CHROME)
	_in_label.add_theme_color_override("font_color", DshTokens.text_secondary())
	_out_label.add_theme_color_override("font_color", DshTokens.text_secondary())
	_in_label.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
	_out_label.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
	var code := DshThemeBuilder.code_font()
	_in_text.add_theme_font_override("font", code)
	_out_text.add_theme_font_override("font", code)
	_in_text.add_theme_font_size_override("font_size", DshTokens.FONT_CODE)
	_out_text.add_theme_font_size_override("font_size", DshTokens.FONT_CODE)
	_in_text.add_theme_color_override("font_color", DshTokens.text_primary())
	_out_text.add_theme_color_override("font_color", DshTokens.text_primary())
	var code_box := DshTokens.box(DshTokens.bg_code(), DshTokens.RADIUS_LG, DshTokens.border_l1(), 1, Vector4(12, 10, 12, 10))
	_in_text.add_theme_stylebox_override("normal", code_box)
	_out_text.add_theme_stylebox_override("normal", code_box)
	DshIcons.apply(_close_icon, "close", 14.0)
	var empty := DshTokens.box(Color(0, 0, 0, 0), DshTokens.RADIUS_PILL, Color(0, 0, 0, 0), 0, Vector4(4, 4, 4, 4))
	var hover := DshTokens.box(DshTokens.bg_layer2(), DshTokens.RADIUS_PILL, Color(0, 0, 0, 0), 0, Vector4(4, 4, 4, 4))
	_close.add_theme_stylebox_override("normal", empty)
	_close.add_theme_stylebox_override("hover", hover)
	_close.add_theme_stylebox_override("pressed", hover)
	_close.flat = true
	_apply_strings()


func show_tool(name: String, input_text: String, output_text: String) -> void:
	if name.strip_edges() == "" and input_text.strip_edges() == "" and output_text.strip_edges() == "":
		_show_empty(true)
		return
	_show_empty(false)
	_title.text = name if name != "" else _t("details.title", "Details")
	_in_text.text = input_text
	_out_text.text = output_text


func set_collapsed(collapsed: bool) -> void:
	visible = not collapsed


func _show_empty(empty: bool) -> void:
	_empty.visible = empty
	_body.visible = not empty
	if empty:
		_title.text = _t("details.title", "Details")


func _apply_strings() -> void:
	if _empty.visible:
		_title.text = _t("details.title", "Details")
	_empty.text = _t("details.empty", "Click a tool row in the message flow to view its details")
	_in_label.text = _t("details.input", "Input")
	_out_label.text = _t("details.output", "Output")
	_close.tooltip_text = _t("details.close", "Close details")


func _t(key: String, fallback: String) -> String:
	var v := str(DshI18n.t(key))
	return fallback if v == key or v.strip_edges() == "" else v
