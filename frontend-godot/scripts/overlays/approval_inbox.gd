extends PanelContainer
class_name ApprovalInbox

## Compact approval inbox strip: the always-visible, non-blocking face of the
## DshApprovalCenter for the main window.
##
## UX contract (replaces the old "every approval steals the screen" flow):
##   * visible only while pending > 0; badge shows the pending count;
##   * expanding (badge click) lists the pending entries as rows;
##   * clicking a row emits [signal entry_clicked](callId) — the app routes to
##     the owning session first, then opens the decision card for that call;
##   * no pending requests -> the whole strip auto-hides (zero visual noise).
##
## Data comes exclusively from the injected approval center (fed by real
## host/permission-request events); this view holds no business state.
## The center is typed loosely (untyped var) so this file compiles standalone
## without depending on cross-file global class caching.

signal entry_clicked(call_id: String)
signal count_changed(count: int)

var _center = null
var _badge: Button = null
var _list: VBoxContainer = null
var _open := false
var _last_auto_denied := ""


func _ready() -> void:
	visible = false
	_build()


func _build() -> void:
	var box := VBoxContainer.new()
	box.add_theme_constant_override("separation", 6)
	add_child(box)

	_badge = Button.new()
	_badge.custom_minimum_size = Vector2(0, 28)
	_badge.clip_text = true
	_badge.alignment = HORIZONTAL_ALIGNMENT_LEFT
	_badge.text_overrun_behavior = TextServer.OVERRUN_TRIM_ELLIPSIS
	_badge.mouse_default_cursor_shape = Control.CURSOR_POINTING_HAND
	_badge.pressed.connect(toggle_expanded)
	box.add_child(_badge)

	var scroll := ScrollContainer.new()
	scroll.custom_minimum_size = Vector2(340, 132)
	scroll.horizontal_scroll_mode = ScrollContainer.SCROLL_MODE_DISABLED
	box.add_child(scroll)

	_list = VBoxContainer.new()
	_list.add_theme_constant_override("separation", 4)
	scroll.add_child(_list)
	_apply_style()


## Visual pass (redesign standard, task #18): floating elevated card, pill
## badge with count, tinted hover rows instead of flat buttons — the inbox
## should read as light chrome that never competes with the conversation.
func _apply_style() -> void:
	add_theme_stylebox_override("panel", DshTokens.elevated(
		DshTokens.bg_layer1(),
		DshTokens.RADIUS_LG,
		Vector4(12, 10, 12, 10),
		2
	))
	if _badge != null:
		_badge.flat = true
		_badge.add_theme_color_override("font_color", DshTokens.warn())
		_badge.add_theme_font_size_override("font_size", DshTokens.FONT_CHROME)
		var rest := DshTokens.box(Color(0, 0, 0, 0), DshTokens.RADIUS_PILL, Color(0, 0, 0, 0), 0, Vector4(6, 4, 10, 4))
		var hov := DshTokens.box(DshTokens.bg_layer2(), DshTokens.RADIUS_PILL, Color(0, 0, 0, 0), 0, Vector4(10, 4, 10, 4))
		_badge.add_theme_stylebox_override("normal", rest)
		_badge.add_theme_stylebox_override("hover", hov)
		_badge.add_theme_stylebox_override("pressed", hov)


func set_center(center) -> void:
	if _center == center:
		return
	if _center != null:
		if _center.changed.is_connected(_refresh):
			_center.changed.disconnect(_refresh)
		if _center.has_signal("auto_decision") and _center.auto_decision.is_connected(_on_auto_decision):
			_center.auto_decision.disconnect(_on_auto_decision)
	_center = center
	if _center != null:
		_center.changed.connect(_refresh)
		if _center.has_signal("auto_decision"):
			_center.auto_decision.connect(_on_auto_decision)
	_refresh()


func _on_auto_decision(call_id: String, decision: String) -> void:
	if decision != "deny" or _center == null:
		return
	var e = _center.get_item(call_id)
	if e == null:
		return
	var prompt := str(e.prompt).strip_edges().split("\n")[0]
	if prompt.length() > 56:
		prompt = prompt.substr(0, 48) + "…"
	_last_auto_denied = prompt
	_apply_auto_tooltip()


func _refresh() -> void:
	if _center == null:
		visible = false
		return
	var pending: Array = _center.pending()
	var n := int(pending.size())
	count_changed.emit(n)
	if n == 0:
		_open = false
		visible = false
		return
	visible = true
	_rebuild_rows(pending)
	_apply_auto_tooltip()


func _rebuild_rows(pending: Array) -> void:
	if _badge != null:
		_badge.text = _badge_text(pending.size())
		_badge.visible = true
	for c in _list.get_children():
		_list.remove_child(c)
		c.queue_free()
	for e in pending:
		var btn := Button.new()
		btn.clip_text = true
		# 行卡片：左缘警示竖条用 border_width 表达，圆角悬停面替代平面按钮。
		btn.custom_minimum_size = Vector2(0, 30)
		btn.alignment = HORIZONTAL_ALIGNMENT_LEFT
		btn.text_overrun_behavior = TextServer.OVERRUN_TRIM_ELLIPSIS
		btn.mouse_default_cursor_shape = Control.CURSOR_POINTING_HAND
		btn.text = _entry_text(e)
		var cid := str(e.call_id)
		btn.pressed.connect(func() -> void: entry_clicked.emit(cid))
		_style_row(btn)
		_list.add_child(btn)
	_list.visible = _open


## 行视觉：左缘 3px 警示色竖条 + 圆角行面；hover 抬亮、pressed 下压，
## 与会话列表的行语言一致（同一套 token，不再临时配色）。
func _style_row(btn: Button) -> void:
	btn.flat = true
	var rest := DshTokens.box(Color(0, 0, 0, 0), DshTokens.RADIUS_SM, Color(0, 0, 0, 0), 0, Vector4(10, 6, 8, 6))
	var hov := DshTokens.box(DshTokens.bg_layer2(), DshTokens.RADIUS_MD, Color(0, 0, 0, 0), 0, Vector4(10, 6, 8, 6))
	var prs := DshTokens.box(DshTokens.pressed_layer(), DshTokens.RADIUS_SM, Color(0, 0, 0, 0), 0, Vector4(12, 6, 8, 6))
	btn.add_theme_stylebox_override("normal", rest)
	btn.add_theme_stylebox_override("hover", hov)
	btn.add_theme_stylebox_override("pressed", prs)
	btn.add_theme_stylebox_override("focus", rest)
	btn.add_theme_color_override("font_color", DshTokens.text_secondary())
	btn.add_theme_color_override("font_hover_color", DshTokens.text_primary())
	btn.add_theme_font_size_override("font_size", DshTokens.FONT_CHROME)


func toggle_expanded() -> void:
	_open = not _open
	if _list != null:
		_list.visible = _open


func expanded() -> bool:
	return _open


func _entry_text(e) -> String:
	var prompt := str(e.prompt).strip_edges().split("\n")[0]
	if prompt.length() > 56:
		prompt = prompt.substr(0, 48) + "…"
	return prompt


func _badge_text(count: int) -> String:
	return "⚠ %d pending approval%s" % [count, "" if count == 1 else "s"]


func _apply_auto_tooltip() -> void:
	if _badge == null or _last_auto_denied == "":
		return
	_badge.tooltip_text = "%s — %s" % [_t("approval.autoDenied", "Permission request auto-denied (turn continues)."), _last_auto_denied]


func _t(key: String, fallback: String) -> String:
	var v := str(DshI18n.t(key))
	return fallback if v == key or v.strip_edges() == "" else v