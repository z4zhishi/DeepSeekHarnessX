extends CanvasLayer
class_name SessionSwitcher

## Quick session switcher. Built entirely in code so `SessionSwitcher.new()`
## works without a scene. Parent instances this; it is not in app.tscn.

signal session_picked(id: String)

var _sessions: Array = []
var _active_id := ""
var _pinned: PackedStringArray = PackedStringArray()
var _opened := false

var _backdrop: ColorRect
var _stage: Control
var _card: PanelContainer
var _title: Label
var _search: LineEdit
var _list: ItemList
var _hint: Label


func _ready() -> void:
	layer = 30
	process_mode = Node.PROCESS_MODE_ALWAYS
	_build()
	if not _opened:
		visible = false
	if get_node_or_null("/root/DshI18n") != null and DshI18n.has_signal("locale_changed"):
		DshI18n.locale_changed.connect(func(_loc: String) -> void: _apply_strings())


func _build() -> void:
	if _card != null:
		return
	layer = 30

	_backdrop = ColorRect.new()
	_backdrop.color = Color(0, 0, 0, 0.45)
	_backdrop.set_anchors_preset(Control.PRESET_FULL_RECT)
	_backdrop.mouse_filter = Control.MOUSE_FILTER_STOP
	_backdrop.gui_input.connect(_on_backdrop)
	add_child(_backdrop)

	# Full-rect Control (not CenterContainer): slide_in_y tweens position, and a
	# CenterContainer would fight that every layout pass (handoff §5.9).
	_stage = Control.new()
	_stage.set_anchors_preset(Control.PRESET_FULL_RECT)
	_stage.mouse_filter = Control.MOUSE_FILTER_IGNORE
	add_child(_stage)

	_card = PanelContainer.new()
	_card.custom_minimum_size = Vector2(440, 320)
	_card.mouse_filter = Control.MOUSE_FILTER_STOP
	_place_card()
	_stage.add_child(_card)

	var box := VBoxContainer.new()
	box.add_theme_constant_override("separation", 10)
	_card.add_child(box)

	_title = Label.new()
	_title.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_title.add_theme_font_size_override("font_size", DshTokens.FONT_CHROME_LG)
	box.add_child(_title)

	_search = LineEdit.new()
	_search.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_search.text_changed.connect(func(_t: String) -> void: _rebuild())
	_search.text_submitted.connect(func(_t: String) -> void: _pick_current())
	box.add_child(_search)

	_list = ItemList.new()
	_list.size_flags_vertical = Control.SIZE_EXPAND_FILL
	_list.custom_minimum_size = Vector2(0, 220)
	_list.item_clicked.connect(_on_item_clicked)
	box.add_child(_list)

	_hint = Label.new()
	_hint.autowrap_mode = TextServer.AUTOWRAP_WORD_SMART
	box.add_child(_hint)

	_apply_style()
	_apply_strings()


func open(sessions: Array, active_id: String, pinned: PackedStringArray) -> void:
	_sessions = sessions
	_active_id = active_id
	_pinned = pinned
	_opened = true
	_build()
	if _search != null:
		_search.text = ""
	visible = true
	_rebuild()
	_apply_style()
	if is_inside_tree():
		call_deferred("_play_open_motion")
		call_deferred("_grab_search")
	else:
		_play_open_motion()


func close() -> void:
	_opened = false
	visible = false
	if _card != null:
		_card.modulate.a = 1.0
	if _backdrop != null:
		_backdrop.modulate.a = 1.0


## Center the card with anchors + offsets derived from its minimum size.
## Position is then owned by the control itself, so DshTokens.slide_in_y can
## tween it without a container writing position back every frame.
func _place_card() -> void:
	if _card == null:
		return
	var sz := _card.custom_minimum_size
	if sz.x < 1.0:
		sz.x = 440.0
	if sz.y < 1.0:
		sz.y = 320.0
	_card.anchor_left = 0.5
	_card.anchor_top = 0.5
	_card.anchor_right = 0.5
	_card.anchor_bottom = 0.5
	_card.grow_horizontal = Control.GROW_DIRECTION_BOTH
	_card.grow_vertical = Control.GROW_DIRECTION_BOTH
	_card.offset_left = -sz.x * 0.5
	_card.offset_top = -sz.y * 0.5
	_card.offset_right = sz.x * 0.5
	_card.offset_bottom = sz.y * 0.5


func _grab_search() -> void:
	if _search != null and visible:
		_search.grab_focus()
		_search.select_all()


func _play_open_motion() -> void:
	if not visible:
		return
	if _backdrop != null:
		var bc := _backdrop.modulate
		bc.a = 1.0
		_backdrop.modulate = bc
		DshTokens.fade_in(_backdrop, DshTokens.MOTION_BASE)
	if _card != null:
		var cc := _card.modulate
		cc.a = 1.0
		_card.modulate = cc
		DshTokens.slide_in_y(_card, 12.0, DshTokens.MOTION_SNAP)


func _apply_style() -> void:
	if _card != null:
		_card.add_theme_stylebox_override("panel", DshTokens.elevated(
			DshTokens.bg_layer1(),
			DshTokens.RADIUS_LG,
			Vector4(18, 16, 18, 16),
			3
		))
	if _title != null:
		_title.add_theme_color_override("font_color", DshTokens.text_primary())
	if _hint != null:
		_hint.add_theme_color_override("font_color", DshTokens.text_tertiary())
		_hint.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
	if _search != null:
		_search.add_theme_color_override("font_color", DshTokens.text_primary())
		_search.add_theme_color_override("font_placeholder_color", DshTokens.text_tertiary())
		_search.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
		_search.add_theme_stylebox_override("normal", DshTokens.box(DshTokens.bg_layer2(), DshTokens.RADIUS_PILL, DshTokens.border_l1(), 1, Vector4(10, 4, 10, 4)))
		_search.add_theme_stylebox_override("focus", DshTokens.box(DshTokens.bg_layer2(), DshTokens.RADIUS_PILL, DshTokens.accent(), 1, Vector4(10, 4, 10, 4)))
	if _list != null:
		_list.add_theme_stylebox_override("panel", DshTokens.box(Color(0, 0, 0, 0), 0, Color(0, 0, 0, 0), 0, Vector4(0, 0, 0, 0)))
		_list.add_theme_stylebox_override("focus", DshTokens.box(Color(0, 0, 0, 0), 0, Color(0, 0, 0, 0), 0, Vector4(0, 0, 0, 0)))
		_list.add_theme_stylebox_override("hovered", DshTokens.box(DshTokens.bg_layer2(), DshTokens.RADIUS_SM, Color(0, 0, 0, 0), 0, Vector4(8, 5, 10, 5)))
		_list.add_theme_stylebox_override("selected", DshTokens.box(DshTokens.accent_soft(), DshTokens.RADIUS_MD, Color(0, 0, 0, 0), 0, Vector4(10, 5, 10, 5)))
		_list.add_theme_color_override("font_color", DshTokens.text_secondary())
		_list.add_theme_color_override("font_hovered_color", DshTokens.text_primary())
		_list.add_theme_color_override("font_selected_color", DshTokens.text_primary())


func _apply_strings() -> void:
	if _title != null:
		_title.text = _t("app.sessionSwitcher", "切换会话")
	if _search != null:
		_search.placeholder_text = _t("app.searchSessions", "搜索会话…")
	if _hint != null:
		_hint.text = _t("app.sessionSwitcherHint", "搜索标题或工作区，Enter 打开")


func _rebuild() -> void:
	if _list == null:
		return
	_list.clear()
	var q := ""
	if _search != null:
		q = _search.text.strip_edges().to_lower()
	var seen: Dictionary = {}
	var select := -1
	for pid in _pinned:
		var s := _session_by_id(pid)
		if s.is_empty() or not _matches(s, q):
			continue
		var idx := _add_row(s, true)
		seen[pid] = true
		if pid == _active_id:
			select = idx
	for raw in _sessions:
		if not (raw is Dictionary):
			continue
		var session := raw as Dictionary
		var id := str(session.get("id", ""))
		if id == "" or seen.has(id) or not _matches(session, q):
			continue
		var idx := _add_row(session, false)
		if id == _active_id:
			select = idx
	if select < 0 and _list.item_count > 0:
		select = 0
	if select >= 0:
		_list.select(select)


func _add_row(session: Dictionary, pinned: bool) -> int:
	var id := str(session.get("id", ""))
	_list.add_item(_row_text(session, pinned))
	var idx := _list.item_count - 1
	_list.set_item_metadata(idx, {"kind": "session", "id": id})
	return idx


func _row_text(session: Dictionary, pinned: bool) -> String:
	var title := _session_title(session)
	var cwd := str(session.get("cwd", "")).strip_edges().replace("\\", "/")
	var extra := cwd.get_file() if cwd != "" else ""
	if extra == "" and cwd != "":
		extra = cwd
	var body := title
	if extra != "" and extra != title:
		body = "%s  ·  %s" % [title, extra]
	return ("• " + body) if pinned else body


func _session_by_id(id: String) -> Dictionary:
	for raw in _sessions:
		if raw is Dictionary and str((raw as Dictionary).get("id", "")) == id:
			return raw
	return {}


func _session_title(s: Dictionary) -> String:
	var title := str(s.get("title", ""))
	if title != "":
		return title
	var cwd := str(s.get("cwd", "")).strip_edges().replace("\\", "/")
	if cwd != "":
		var base := cwd.get_file()
		return base if base != "" else cwd
	var id := str(s.get("id", ""))
	return id.substr(0, 8) if id.length() > 8 else id


func _matches(s: Dictionary, q: String) -> bool:
	if q == "":
		return true
	if _session_title(s).to_lower().find(q) >= 0:
		return true
	for key in ["title", "id", "cwd"]:
		if str(s.get(key, "")).to_lower().find(q) >= 0:
			return true
	return false


func _on_item_clicked(index: int, _at: Vector2, button_index: int) -> void:
	if button_index == MOUSE_BUTTON_LEFT:
		_pick_index(index)


func _on_backdrop(event: InputEvent) -> void:
	if event is InputEventMouseButton:
		var mb := event as InputEventMouseButton
		if mb.pressed and mb.button_index == MOUSE_BUTTON_LEFT:
			close()


func _pick_current() -> void:
	if _list == null or _list.item_count == 0:
		return
	var sel := _list.get_selected_items()
	var idx := sel[0] if sel.size() > 0 else 0
	_pick_index(idx)


func _pick_index(index: int) -> void:
	if _list == null or index < 0 or index >= _list.item_count:
		return
	var meta: Variant = _list.get_item_metadata(index)
	if meta is Dictionary:
		_pick_id(str(meta.get("id", "")))


func _pick_id(id: String) -> void:
	if not visible or id == "":
		return
	close()
	session_picked.emit(id)


func _move_selection(delta: int) -> void:
	if _list == null or _list.item_count == 0:
		return
	var sel := _list.get_selected_items()
	var cur := sel[0] if sel.size() > 0 else 0
	var next := posmod(cur + delta, _list.item_count)
	_list.select(next)
	_list.ensure_current_is_visible()


func _input(event: InputEvent) -> void:
	if not visible:
		return
	var k := event as InputEventKey
	if k == null or not k.pressed or k.echo:
		return
	match k.keycode:
		KEY_ESCAPE:
			close()
			get_viewport().set_input_as_handled()
		KEY_UP:
			_move_selection(-1)
			get_viewport().set_input_as_handled()
		KEY_DOWN:
			_move_selection(1)
			get_viewport().set_input_as_handled()
		KEY_ENTER, KEY_KP_ENTER:
			_pick_current()
			get_viewport().set_input_as_handled()


func _t(key: String, fallback: String) -> String:
	var v := str(DshI18n.t(key))
	return fallback if v == key or v.strip_edges() == "" else v
