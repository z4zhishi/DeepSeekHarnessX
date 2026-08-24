extends MarginContainer
class_name AssistantRow

signal feedback_rating(message_id: String, rating: String)

@onready var stack: VBoxContainer = %Stack
@onready var copy_btn: Button = %CopyBtn
@onready var like_btn: Button = %LikeBtn
@onready var dislike_btn: Button = %DislikeBtn
@onready var actions: HBoxContainer = %Actions
@onready var usage_caption: Label = %UsageCaption

var _plain: String = ""
var _message_id: String = ""
var _rating: String = ""
var _seg_count: int = -1
var _pending_bind: Dictionary = {}

const ICON_COPY := "res://assets/icons/icon_copy.svg"
const ICON_LIKE := "res://assets/icons/icon_like.svg"
const ICON_LIKE_F := "res://assets/icons/icon_like_fill.svg"
const ICON_DIS := "res://assets/icons/icon_dislike.svg"
const ICON_DIS_F := "res://assets/icons/icon_dislike_fill.svg"


func _ready() -> void:
	add_theme_constant_override("margin_left", 8)
	add_theme_constant_override("margin_right", 8)
	add_theme_constant_override("margin_top", 2)
	add_theme_constant_override("margin_bottom", 2)
	if copy_btn == null or like_btn == null or dislike_btn == null:
		return
	_style_icon_btn(copy_btn, ICON_COPY, _t("chat.copy", "Copy"))
	_style_icon_btn(like_btn, ICON_LIKE, _t("chat.likeTooltip", "Good response"))
	_style_icon_btn(dislike_btn, ICON_DIS, _t("chat.dislikeTooltip", "Bad response"))
	_style_usage()
	if not copy_btn.pressed.is_connected(_on_copy):
		copy_btn.pressed.connect(_on_copy)
	if not like_btn.pressed.is_connected(_on_like):
		like_btn.pressed.connect(_on_like)
	if not dislike_btn.pressed.is_connected(_on_dislike):
		dislike_btn.pressed.connect(_on_dislike)
	if not _pending_bind.is_empty():
		var node := _pending_bind
		_pending_bind = {}
		bind(node)


func bind(node: Dictionary) -> void:
	if copy_btn == null or actions == null or stack == null:
		_pending_bind = node
		return
	var p: Dictionary = node.get("payload", {}) if node.get("payload") is Dictionary else {}
	_message_id = str(p.get("messageId", node.get("id", "")))
	_rating = str(p.get("rating", ""))
	_plain = str(p.get("text", ""))
	_rebuild(_plain)
	_paint_rating()
	var show_actions := _plain.strip_edges() != "" and not bool(p.get("streaming", false))
	actions.visible = show_actions
	_style_usage()
	_apply_usage(p if show_actions else {})


func set_stream_text(text: String) -> void:
	if stack == null:
		return
	_plain = text
	var segs := DshMarkdown.segments(text)
	if segs.size() == _seg_count and stack.get_child_count() == segs.size() and segs.size() > 0:
		var last: Dictionary = segs[segs.size() - 1]
		if str(last.get("kind", "")) == "md":
			var n := stack.get_child(stack.get_child_count() - 1)
			if n is RichTextLabel:
				(n as RichTextLabel).text = DshMarkdown.to_bbcode(str(last.get("text", "")))
				if actions:
					actions.visible = false
				if usage_caption:
					usage_caption.visible = false
				return
	_rebuild(text)
	if actions:
		actions.visible = false
	if usage_caption:
		usage_caption.visible = false


func _rebuild(text: String) -> void:
	if stack == null:
		return
	for c in stack.get_children():
		stack.remove_child(c)
		c.queue_free()
	var segs := DshMarkdown.segments(text)
	_seg_count = segs.size()
	for s in segs:
		if str(s.get("kind", "")) == "code":
			stack.add_child(_code_panel(str(s.get("lang", "")), str(s.get("text", ""))))
		else:
			stack.add_child(_md_label(DshMarkdown.to_bbcode(str(s.get("text", "")))))


func _md_label(bb: String) -> RichTextLabel:
	var rtl := RichTextLabel.new()
	rtl.bbcode_enabled = true
	rtl.fit_content = true
	rtl.scroll_active = false
	rtl.autowrap_mode = TextServer.AUTOWRAP_WORD_SMART
	rtl.selection_enabled = true
	rtl.add_theme_font_size_override("normal_font_size", DshTokens.FONT_BODY)
	rtl.add_theme_color_override("default_color", DshTokens.text_primary())
	rtl.add_theme_font_override("mono_font", DshThemeBuilder.code_font())
	rtl.add_theme_constant_override("line_separation", DshTokens.FONT_BODY_LH - DshTokens.FONT_BODY)
	rtl.meta_underlined = false
	rtl.text = bb
	return rtl


func _code_panel(lang: String, code: String) -> PanelContainer:
	var panel := PanelContainer.new()
	panel.add_theme_stylebox_override("panel", DshTokens.box(
		DshTokens.bg_code(),
		DshTokens.RADIUS_MD,
		DshTokens.border_l2(),
		1,
		Vector4(10, 8, 10, 8)
	))
	var col := VBoxContainer.new()
	col.add_theme_constant_override("separation", 6)
	panel.add_child(col)
	var head := HBoxContainer.new()
	col.add_child(head)
	var lab := Label.new()
	lab.text = lang.to_upper() if lang != "" else "CODE"
	lab.add_theme_font_size_override("font_size", DshTokens.FONT_MICRO)
	lab.add_theme_color_override("font_color", DshTokens.text_tertiary())
	lab.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	head.add_child(lab)
	var btn := Button.new()
	_style_icon_btn(btn, ICON_COPY, _t("chat.copy", "Copy"))
	var captured := code
	btn.pressed.connect(func():
		DisplayServer.clipboard_set(captured)
		btn.tooltip_text = _t("chat.copied", "Copied")
	)
	head.add_child(btn)
	var rtl := RichTextLabel.new()
	rtl.bbcode_enabled = true
	rtl.fit_content = true
	rtl.scroll_active = false
	rtl.autowrap_mode = TextServer.AUTOWRAP_WORD_SMART
	rtl.selection_enabled = true
	rtl.add_theme_font_override("normal_font", DshThemeBuilder.code_font())
	rtl.add_theme_font_override("mono_font", DshThemeBuilder.code_font())
	rtl.add_theme_font_size_override("normal_font_size", DshTokens.FONT_CODE)
	rtl.add_theme_color_override("default_color", DshTokens.text_primary())
	rtl.text = "[code]%s[/code]" % DshMarkdown.escape(code)
	col.add_child(rtl)
	return panel


func _style_icon_btn(btn: Button, icon_path: String, tip: String) -> void:
	if btn == null:
		return
	btn.flat = true
	btn.icon = load(icon_path) as Texture2D
	btn.tooltip_text = tip
	btn.focus_mode = Control.FOCUS_NONE
	btn.custom_minimum_size = Vector2(28, 28)
	var empty := StyleBoxEmpty.new()
	btn.add_theme_stylebox_override("normal", empty)
	btn.add_theme_stylebox_override("hover", empty)
	btn.add_theme_stylebox_override("pressed", empty)
	btn.add_theme_stylebox_override("focus", empty)
	btn.modulate = DshTokens.text_tertiary()


func _style_usage() -> void:
	if usage_caption == null:
		return
	usage_caption.add_theme_color_override("font_color", DshTokens.text_tertiary())
	usage_caption.add_theme_font_size_override("font_size", DshTokens.FONT_MICRO)


func _apply_usage(p: Dictionary) -> void:
	if usage_caption == null:
		return
	var cap := ""
	if p.get("usage") is Dictionary:
		cap = _format_usage(p["usage"] as Dictionary)
	usage_caption.text = cap
	usage_caption.visible = cap != ""


func _format_usage(usage: Dictionary) -> String:
	var inn := _token_count(usage, ["inputTokens", "InputTokens", "input_tokens", "prompt_tokens"])
	var out := _token_count(usage, ["outputTokens", "OutputTokens", "output_tokens", "completion_tokens"])
	if inn <= 0 and out <= 0:
		return ""
	return "↑%d · ↓%d" % [inn, out]


func _token_count(usage: Dictionary, keys: Array[String]) -> int:
	for k in keys:
		if usage.has(k):
			return int(usage[k])
	return 0


func _on_copy() -> void:
	DisplayServer.clipboard_set(_plain)
	if copy_btn:
		copy_btn.tooltip_text = _t("chat.copied", "Copied")


func _on_like() -> void:
	_rating = "" if _rating == "like" else "like"
	_paint_rating()
	feedback_rating.emit(_message_id, _rating)


func _on_dislike() -> void:
	_rating = "" if _rating == "dislike" else "dislike"
	_paint_rating()
	feedback_rating.emit(_message_id, _rating)


func _paint_rating() -> void:
	if like_btn == null or dislike_btn == null:
		return
	like_btn.icon = load(ICON_LIKE_F if _rating == "like" else ICON_LIKE) as Texture2D
	dislike_btn.icon = load(ICON_DIS_F if _rating == "dislike" else ICON_DIS) as Texture2D
	like_btn.modulate = DshTokens.accent() if _rating == "like" else DshTokens.text_tertiary()
	dislike_btn.modulate = DshTokens.danger() if _rating == "dislike" else DshTokens.text_tertiary()


func _t(key: String, fallback: String) -> String:
	var s := DshI18n.t(key)
	return fallback if s == key else s
