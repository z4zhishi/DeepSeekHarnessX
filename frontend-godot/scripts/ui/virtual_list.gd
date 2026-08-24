extends ScrollContainer
class_name ChatList

signal tool_selected(call_id: String, name: String, input: String, output: String)
signal feedback_rating(message_id: String, rating: String)
signal suggestion_clicked(prompt: String)

const OVERSCAN := 400.0
const GAP := 12.0
const PAD_Y := 16.0
const PAD_X := 12.0
const POOL_MAX := 24

const SCENE_USER := preload("res://scenes/rows/user_row.tscn")
const SCENE_ASST := preload("res://scenes/rows/assistant_row.tscn")
const SCENE_REASON := preload("res://scenes/rows/reasoning_row.tscn")
const SCENE_TOOL := preload("res://scenes/rows/tool_row.tscn")
const SCENE_SYS := preload("res://scenes/rows/system_row.tscn")
const SCENE_TODO := preload("res://scenes/cards/todo_card.tscn")
const SCENE_PLAN := preload("res://scenes/cards/plan_card.tscn")
const SCENE_GOAL := preload("res://scenes/cards/goal_card.tscn")

var _fold := ConversationFold.new()
var _nodes: Array = []
var _heights: PackedFloat32Array = PackedFloat32Array()
var _content: Control
var _hero: Control
var _hero_wanted: bool = true
var _pool: Dictionary = {}
var _live: Dictionary = {}
var _sync_pending: bool = false
var _measuring: bool = false
var _measure_q: Array[int] = []
var _stick_bottom: bool = true
var _built: bool = false


func _notification(what: int) -> void:
	if what == NOTIFICATION_THEME_CHANGED and _built:
		if _hero:
			var vis := _hero.visible
			_content.remove_child(_hero)
			_hero.queue_free()
			_hero = null
			_build_hero()
			_hero.visible = vis
			_layout_hero()
		for idx in _live.keys():
			_bind_row(_live[idx], int(idx))


func _ready() -> void:
	horizontal_scroll_mode = ScrollContainer.SCROLL_MODE_DISABLED
	vertical_scroll_mode = ScrollContainer.SCROLL_MODE_AUTO
	_content = Control.new()
	_content.name = "Content"
	_content.mouse_filter = Control.MOUSE_FILTER_PASS
	add_child(_content)
	_build_hero()
	get_v_scroll_bar().value_changed.connect(_on_scroll)
	resized.connect(func(): _request_sync())
	_built = true
	_update_hero()
	_request_sync()


func set_nodes(nodes: Array) -> void:
	_unmount_all()
	_fold.adopt(nodes)
	_nodes = _fold.nodes()
	_rebuild_heights()
	_stick_bottom = true
	_update_hero()
	_request_sync()
	call_deferred("_scroll_bottom")


func apply_event(env: Dictionary) -> void:
	var typ := str(env.get("type", ""))
	_fold.ingest(env)
	_nodes = _fold.nodes()
	_ensure_height_len()
	_update_hero()
	if typ == "assistant/chunk":
		_patch_stream()
	else:
		for idx in _live.keys():
			var i := int(idx)
			if i >= 0 and i < _nodes.size():
				_bind_row(_live[idx], i)
				_queue_measure(i)
		_request_sync()
	if _stick_bottom:
		call_deferred("_scroll_bottom")


func clear() -> void:
	_unmount_all()
	_fold.reset()
	_nodes = _fold.nodes()
	_heights = PackedFloat32Array()
	_measure_q.clear()
	_stick_bottom = true
	_update_hero()
	if _content:
		_content.custom_minimum_size = Vector2.ZERO
	_request_sync()


func is_empty() -> bool:
	return _nodes.is_empty()


func show_hero(visible: bool) -> void:
	_hero_wanted = visible
	_update_hero()


func _on_scroll(_v: float) -> void:
	var bar := get_v_scroll_bar()
	_stick_bottom = bar.value + size.y >= bar.max_value - 64.0
	_request_sync()


func _request_sync() -> void:
	if _sync_pending:
		return
	_sync_pending = true
	call_deferred("_sync")


func _sync() -> void:
	_sync_pending = false
	if not _built:
		return
	_layout_hero()
	if _nodes.is_empty():
		_unmount_all()
		if _hero_wanted and _hero:
			_content.custom_minimum_size = Vector2(size.x, maxf(size.y, 1.0))
		return
	var col := _column()
	var prefix := PackedFloat32Array()
	prefix.resize(_nodes.size() + 1)
	prefix[0] = PAD_Y
	for i in _nodes.size():
		prefix[i + 1] = prefix[i] + _heights[i] + GAP
	var total := prefix[_nodes.size()] - GAP + PAD_Y
	_content.custom_minimum_size = Vector2(size.x, maxf(total, 1.0))
	var scroll_y := float(scroll_vertical)
	var view_h := size.y
	var lo := scroll_y - OVERSCAN
	var hi := scroll_y + view_h + OVERSCAN
	var want: Dictionary = {}
	for i in _nodes.size():
		var y := prefix[i]
		var h := _heights[i]
		if y + h >= lo and y <= hi:
			want[i] = true
	for idx in _live.keys():
		if not want.has(idx):
			_release(int(idx))
	for idx in want.keys():
		var i := int(idx)
		var row: Control
		if _live.has(i):
			row = _live[i]
		else:
			row = _acquire(str(_nodes[i].get("kind", "system")))
			_live[i] = row
			_content.add_child(row)
			_bind_row(row, i)
			_queue_measure(i)
		row.position = Vector2(col.x, prefix[i])
		row.custom_minimum_size = Vector2(col.y, 0)
		row.size = Vector2(col.y, maxf(_heights[i], 1.0))


func _patch_stream() -> void:
	var start := maxi(0, _nodes.size() - 6)
	var any_live := false
	for i in range(start, _nodes.size()):
		var kind := str(_nodes[i].get("kind", ""))
		if kind != "assistant" and kind != "reasoning":
			continue
		if _live.has(i):
			any_live = true
			var row: Control = _live[i]
			var text := str((_nodes[i].get("payload", {}) as Dictionary).get("text", "")) if _nodes[i].get("payload") is Dictionary else ""
			if row.has_method("set_stream_text"):
				row.call("set_stream_text", text)
			else:
				_bind_row(row, i)
			_queue_measure(i)
	if not any_live:
		_request_sync()
	else:
		_layout_positions()


func _layout_positions() -> void:
	if _nodes.is_empty() or not _built:
		return
	var col := _column()
	var y := PAD_Y
	for i in _nodes.size():
		if _live.has(i):
			var row: Control = _live[i]
			row.position = Vector2(col.x, y)
			row.custom_minimum_size = Vector2(col.y, 0)
			row.size = Vector2(col.y, maxf(_heights[i], 1.0))
		y += _heights[i] + GAP
	_content.custom_minimum_size = Vector2(size.x, maxf(y - GAP + PAD_Y, 1.0))


func _bind_row(row: Control, index: int) -> void:
	if index < 0 or index >= _nodes.size():
		return
	var node: Dictionary = _nodes[index]
	if row.has_method("bind"):
		row.call("bind", node)
	_wire(row, index)


func _wire(row: Control, index: int) -> void:
	_rewire(row, "height_changed", "hcb", _on_row_height.bind(index))
	_rewire(row, "tool_selected", "tcb", _on_tool_selected)
	_rewire(row, "feedback_rating", "fcb", _on_feedback.bind(index))


func _rewire(row: Control, sig: String, meta: String, cb: Callable) -> void:
	if row == null or not is_instance_valid(row) or not row.has_signal(sig):
		return
	if row.has_meta(meta):
		var existing: Variant = row.get_meta(meta)
		if existing is Callable and row.is_connected(sig, existing):
			row.disconnect(sig, existing)
		row.remove_meta(meta)
	row.set_meta(meta, cb)
	if not row.is_connected(sig, cb):
		row.connect(sig, cb)


func _on_row_height(index: int) -> void:
	if index >= 0 and index < _nodes.size():
		var row: Control = _live.get(index, null)
		if row != null and row.has_method("is_expanded"):
			var p: Dictionary = _nodes[index].get("payload", {})
			if p is Dictionary:
				p["expanded"] = bool(row.call("is_expanded"))
				_nodes[index]["payload"] = p
	_queue_measure(index)


func _on_tool_selected(call_id: String, name: String, input: String, output: String) -> void:
	tool_selected.emit(call_id, name, input, output)


func _on_feedback(message_id: String, rating: String, index: int) -> void:
	if index >= 0 and index < _nodes.size():
		var p: Dictionary = _nodes[index].get("payload", {})
		if p is Dictionary:
			p["rating"] = rating
			_nodes[index]["payload"] = p
	feedback_rating.emit(message_id, rating)


func _queue_measure(index: int) -> void:
	if index < 0 or index >= _nodes.size():
		return
	if _measure_q.has(index):
		return
	_measure_q.append(index)
	if not _measuring:
		_run_measure()


func _run_measure() -> void:
	_measuring = true
	while _measure_q.size() > 0:
		var i: int = _measure_q.pop_front()
		if not is_inside_tree() or not _live.has(i) or i < 0 or i >= _nodes.size():
			continue
		var row: Control = _live[i]
		if not is_instance_valid(row) or not row.is_inside_tree():
			continue
		var col := _column()
		row.custom_minimum_size = Vector2(col.y, 0)
		row.size.x = col.y
		await get_tree().process_frame
		if not is_inside_tree() or not _live.has(i) or i < 0 or i >= _nodes.size() or i >= _heights.size():
			continue
		row = _live[i]
		if not is_instance_valid(row) or not row.is_inside_tree():
			continue
		var h := row.get_combined_minimum_size().y
		if h < 1.0:
			h = row.size.y
		if h >= 1.0 and not is_equal_approx(h, _heights[i]):
			_heights[i] = h
			_layout_positions()
	_measuring = false
	if _measure_q.size() > 0:
		_run_measure()


func _acquire(kind: String) -> Control:
	var pool: Array = _pool.get(kind, [])
	if pool.size() > 0:
		return pool.pop_back()
	var scene := _scene_for(kind)
	return scene.instantiate()


func _release(index: int) -> void:
	if not _live.has(index):
		return
	var row: Control = _live[index]
	_live.erase(index)
	_unwire(row)
	var parent := row.get_parent()
	if parent:
		parent.remove_child(row)
	var kind := "system"
	if index < _nodes.size():
		kind = str(_nodes[index].get("kind", "system"))
	if not _pool.has(kind):
		_pool[kind] = []
	var pool: Array = _pool[kind]
	if pool.size() >= POOL_MAX:
		row.queue_free()
	else:
		pool.append(row)


func _unwire(row: Control) -> void:
	if row == null or not is_instance_valid(row):
		return
	for pair in [["height_changed", "hcb"], ["tool_selected", "tcb"], ["feedback_rating", "fcb"]]:
		var sig := str(pair[0])
		var meta := str(pair[1])
		if not row.has_meta(meta):
			continue
		var cb: Variant = row.get_meta(meta)
		if cb is Callable and row.has_signal(sig) and row.is_connected(sig, cb):
			row.disconnect(sig, cb)
		row.remove_meta(meta)


func _unmount_all() -> void:
	var keys: Array = _live.keys()
	for idx in keys:
		_release(int(idx))
	_live.clear()


func _scene_for(kind: String) -> PackedScene:
	match kind:
		"user":
			return SCENE_USER
		"assistant":
			return SCENE_ASST
		"reasoning":
			return SCENE_REASON
		"tool":
			return SCENE_TOOL
		"todo":
			return SCENE_TODO
		"plan":
			return SCENE_PLAN
		"goal":
			return SCENE_GOAL
		_:
			return SCENE_SYS


func _rebuild_heights() -> void:
	_heights.resize(_nodes.size())
	for i in _nodes.size():
		_heights[i] = _estimate(_nodes[i])


func _ensure_height_len() -> void:
	if _heights.size() == _nodes.size():
		return
	if _heights.size() < _nodes.size():
		var old := _heights.size()
		_heights.resize(_nodes.size())
		for i in range(old, _nodes.size()):
			_heights[i] = _estimate(_nodes[i])
	else:
		_heights.resize(_nodes.size())


func _estimate(node: Dictionary) -> float:
	var kind := str(node.get("kind", ""))
	var p: Dictionary = node.get("payload", {}) if node.get("payload") is Dictionary else {}
	match kind:
		"user":
			var t := str(p.get("text", ""))
			var lines := t.count("\n") + 1 + int(t.length() / 56)
			return 40.0 + float(mini(lines, 24)) * 22.0
		"assistant":
			var t := str(p.get("text", ""))
			var lines := t.count("\n") + 1 + int(t.length() / 64)
			return 36.0 + float(mini(lines, 40)) * 22.0
		"reasoning":
			return 88.0 if bool(p.get("expanded", false)) else 28.0
		"tool":
			return 180.0 if bool(p.get("expanded", false)) else 28.0
		"todo", "plan", "goal":
			return 72.0
		"system", "command", "turn-error":
			return 24.0
		_:
			return 40.0


func _column() -> Vector2:
	var inner := maxf(size.x - PAD_X * 2.0, 80.0)
	var w := minf(inner, DshTokens.CHAT_CONTENT_WIDTH)
	var x := (size.x - w) * 0.5
	return Vector2(x, w)


func _scroll_bottom() -> void:
	await get_tree().process_frame
	scroll_vertical = int(_content.custom_minimum_size.y)


func _update_hero() -> void:
	if _hero == null:
		return
	var show := _hero_wanted and _nodes.is_empty()
	_hero.visible = show
	if show:
		_layout_hero()


func _layout_hero() -> void:
	if _hero == null or not _hero.visible:
		return
	var col := _column()
	_hero.position = Vector2(col.x, 0.0)
	_hero.size = Vector2(col.y, maxf(size.y, 1.0))
	_content.custom_minimum_size = Vector2(size.x, maxf(size.y, 1.0))


func _build_hero() -> void:
	_hero = VBoxContainer.new()
	_hero.name = "Hero"
	_hero.alignment = BoxContainer.ALIGNMENT_CENTER
	_hero.add_theme_constant_override("separation", 14)
	_hero.mouse_filter = Control.MOUSE_FILTER_STOP
	_content.add_child(_hero)

	var mark := TextureRect.new()
	mark.texture = load("res://assets/brand/dshx_mark.svg") as Texture2D
	mark.custom_minimum_size = Vector2(48, 48)
	mark.expand_mode = TextureRect.EXPAND_IGNORE_SIZE
	mark.stretch_mode = TextureRect.STRETCH_KEEP_ASPECT_CENTERED
	mark.size_flags_horizontal = Control.SIZE_SHRINK_CENTER
	mark.modulate = DshTokens.text_primary()
	_hero.add_child(mark)

	var title := Label.new()
	title.text = _t("chat.heroTitle", "DSHX")
	title.horizontal_alignment = HORIZONTAL_ALIGNMENT_CENTER
	title.add_theme_font_size_override("font_size", 24)
	title.add_theme_color_override("font_color", DshTokens.text_primary())
	_hero.add_child(title)

	var sub := Label.new()
	sub.text = _t("chat.heroSubtitle", "High-performance agent workbench")
	sub.horizontal_alignment = HORIZONTAL_ALIGNMENT_CENTER
	sub.autowrap_mode = TextServer.AUTOWRAP_WORD_SMART
	sub.add_theme_font_size_override("font_size", DshTokens.FONT_UI)
	sub.add_theme_color_override("font_color", DshTokens.text_tertiary())
	_hero.add_child(sub)

	var grid := GridContainer.new()
	grid.columns = 2
	grid.size_flags_horizontal = Control.SIZE_SHRINK_CENTER
	grid.add_theme_constant_override("h_separation", 12)
	grid.add_theme_constant_override("v_separation", 12)
	_hero.add_child(grid)

	var suggestions := [
		{
			"icon": "icon_folder.svg",
			"title": _t("chat.suggestExplore", "Explore workspace"),
			"desc": _t("chat.suggestExploreDesc", "Index the tree and explain the entry points."),
			"prompt": "Please explore and explain the architecture of this workspace.",
		},
		{
			"icon": "icon_plan.svg",
			"title": _t("chat.suggestPlan", "Make a plan"),
			"desc": _t("chat.suggestPlanDesc", "Draft a multi-step design before editing."),
			"prompt": "/plan on",
		},
		{
			"icon": "icon_diff.svg",
			"title": _t("chat.suggestDiff", "Review diff"),
			"desc": _t("chat.suggestDiffDesc", "Inspect working-tree changes."),
			"prompt": "Check current git diff and review recent changes.",
		},
		{
			"icon": "icon_check.svg",
			"title": _t("chat.suggestTest", "Run tests"),
			"desc": _t("chat.suggestTestDesc", "Execute the test suite and explain failures."),
			"prompt": "Run the project test suite and report results.",
		},
	]
	for s in suggestions:
		grid.add_child(_suggest_card(s))


func _suggest_card(s: Dictionary) -> Control:
	var wrap := PanelContainer.new()
	wrap.custom_minimum_size = Vector2(280, 72)
	var box_n := DshTokens.box(DshTokens.bg_layer2(), DshTokens.RADIUS_MD, DshTokens.border_l2(), 1, Vector4(14, 10, 14, 10))
	var box_h := DshTokens.box(DshTokens.bg_layer3(), DshTokens.RADIUS_MD, DshTokens.border_l3(), 1, Vector4(14, 10, 14, 10))
	wrap.add_theme_stylebox_override("panel", box_n)
	var vbox := VBoxContainer.new()
	vbox.mouse_filter = Control.MOUSE_FILTER_IGNORE
	vbox.add_theme_constant_override("separation", 2)
	wrap.add_child(vbox)
	var head := HBoxContainer.new()
	head.mouse_filter = Control.MOUSE_FILTER_IGNORE
	head.add_theme_constant_override("separation", 6)
	vbox.add_child(head)
	var ic := TextureRect.new()
	ic.texture = load("res://assets/icons/%s" % str(s["icon"])) as Texture2D
	ic.custom_minimum_size = Vector2(16, 16)
	ic.expand_mode = TextureRect.EXPAND_IGNORE_SIZE
	ic.stretch_mode = TextureRect.STRETCH_KEEP_ASPECT_CENTERED
	ic.modulate = DshTokens.text_secondary()
	ic.mouse_filter = Control.MOUSE_FILTER_IGNORE
	head.add_child(ic)
	var tl := Label.new()
	tl.text = str(s["title"])
	tl.add_theme_font_size_override("font_size", DshTokens.FONT_CHROME)
	tl.add_theme_color_override("font_color", DshTokens.text_primary())
	tl.mouse_filter = Control.MOUSE_FILTER_IGNORE
	head.add_child(tl)
	var dl := Label.new()
	dl.text = str(s["desc"])
	dl.add_theme_font_size_override("font_size", DshTokens.FONT_MICRO)
	dl.add_theme_color_override("font_color", DshTokens.text_tertiary())
	dl.mouse_filter = Control.MOUSE_FILTER_IGNORE
	vbox.add_child(dl)
	var btn := Button.new()
	btn.flat = true
	var empty := StyleBoxEmpty.new()
	btn.add_theme_stylebox_override("normal", empty)
	btn.add_theme_stylebox_override("hover", empty)
	btn.add_theme_stylebox_override("pressed", empty)
	btn.add_theme_stylebox_override("focus", empty)
	var prompt := str(s["prompt"])
	btn.pressed.connect(func(): suggestion_clicked.emit(prompt))
	btn.mouse_entered.connect(func(): wrap.add_theme_stylebox_override("panel", box_h))
	btn.mouse_exited.connect(func(): wrap.add_theme_stylebox_override("panel", box_n))
	wrap.add_child(btn)
	return wrap


func _t(key: String, fallback: String) -> String:
	var s := DshI18n.t(key)
	return fallback if s == key else s
