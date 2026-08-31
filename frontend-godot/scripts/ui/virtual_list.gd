extends ScrollContainer
class_name ChatList

signal tool_selected(call_id: String, name: String, input: String, output: String)
signal feedback_rating(message_id: String, rating: String)
# W12-a: 原死路径 `_build_hero`/`show_hero` 与 ChatList 内部 hero 节点已删除。
# 空态唯一出口是 app.gd 的 HeroView（ChatTab 下 ChatList 的兄弟节点），
# 由 `_show_empty()` 切换；ChatList 不再持有第二套空态实现。

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
var _pool: Dictionary = {}
var _live: Dictionary = {}
# 同步调度走单发 Timer（在下一帧的 _process 阶段触发），绝不走 call_deferred：
# 布局变化会经滚动条信号再次请求同步，若在同一轮 MessageQueue flush 内续链，
# 会形成「sync→布局→滚动事件→sync」的自馈循环，把主循环饿死在 flush 里
# （历史事故：加载会话后整帧冻结 15s+，协程全部 starvation）。
var _sync_timer: Timer
var _scroll_programmatic := false
var _stick_bottom: bool = true
var _built: bool = false
var _heights_dirty: bool = false


func _notification(what: int) -> void:
	if what == NOTIFICATION_THEME_CHANGED and _built:
		for idx in _live.keys():
			_bind_row(_live[idx], int(idx))


func _ready() -> void:
	horizontal_scroll_mode = ScrollContainer.SCROLL_MODE_DISABLED
	vertical_scroll_mode = ScrollContainer.SCROLL_MODE_AUTO
	_content = Control.new()
	_content.name = "Content"
	_content.mouse_filter = Control.MOUSE_FILTER_PASS
	add_child(_content)
	_sync_timer = Timer.new()
	_sync_timer.one_shot = true
	_sync_timer.wait_time = 0.02
	_sync_timer.timeout.connect(_sync)
	add_child(_sync_timer)
	get_v_scroll_bar().value_changed.connect(_on_scroll)
	resized.connect(func(): _request_sync())
	_built = true
	_request_sync()


func set_nodes(nodes: Array, seen_seq: Dictionary = {}) -> void:
	_dbg("set_nodes enter n=%d frame=%d" % [nodes.size(), Engine.get_process_frames()])
	if _is_same_nodes(nodes):
		if not seen_seq.is_empty() and _fold.has_method("merge_seen_seq"):
			_fold.merge_seen_seq(seen_seq)
		_dbg("set_nodes skipped identical nodes")
		return
	_unmount_all()
	_fold.adopt(nodes, seen_seq)
	_nodes = _fold.nodes()
	_rebuild_heights()
	_stick_bottom = true
	_sync()
	call_deferred("_scroll_bottom")
	_dbg("set_nodes exit")


func _is_same_nodes(new_nodes: Array) -> bool:
	if new_nodes.size() != _nodes.size():
		return false
	for i in _nodes.size():
		var a: Dictionary = _nodes[i] if _nodes[i] is Dictionary else {}
		var b: Dictionary = new_nodes[i] if new_nodes[i] is Dictionary else {}
		if str(a.get("id", "")) != str(b.get("id", "")) or str(a.get("kind", "")) != str(b.get("kind", "")):
			return false
		var pa: Dictionary = a.get("payload", {}) if a.get("payload") is Dictionary else {}
		var pb: Dictionary = b.get("payload", {}) if b.get("payload") is Dictionary else {}
		if pa != pb:
			return false
	return true


func apply_event(env: Dictionary) -> void:
	var typ := str(env.get("type", ""))
	var before := _nodes.size()
	_fold.ingest(env)
	_nodes = _fold.nodes()
	_ensure_height_len()
	if typ == "assistant/chunk":
		_patch_stream()
	elif typ == "tool/call" or typ == "tool/result":
		_patch_tool(env)
		if _nodes.size() != before:
			_request_sync()
	elif typ == "assistant/message" or typ == "assistant/reasoning":
		_patch_stream()
		_request_sync()
	else:
		_request_sync()
	if _stick_bottom:
		call_deferred("_scroll_bottom")


func clear() -> void:
	_unmount_all()
	_fold.reset()
	_nodes = _fold.nodes()
	_heights = PackedFloat32Array()
	_stick_bottom = true
	if _content:
		_content.custom_minimum_size = Vector2.ZERO
	_request_sync()


func is_empty() -> bool:
	return _nodes.is_empty()


func _on_scroll(_v: float) -> void:
	if _scroll_programmatic:
		return
	var bar := get_v_scroll_bar()
	_stick_bottom = bar.value + size.y >= bar.max_value - 64.0
	_request_sync()


func _request_sync() -> void:
	if not _sync_timer.is_stopped():
		return
	_sync_timer.start()


func _sync() -> void:
	if not _built:
		return
	var ts := Time.get_ticks_msec()
	_dbg("sync enter live=%d nodes=%d" % [_live.size(), _nodes.size()])
	if _nodes.is_empty():
		_unmount_all()
		# 空列表不遗留旧的最小高度，避免幽灵滚动范围（原为 hero 占位逻辑）。
		_content.custom_minimum_size = Vector2.ZERO
		return
	var col := _column()
	var prefix := PackedFloat32Array()
	prefix.resize(_nodes.size() + 1)
	prefix[0] = PAD_Y
	for i in _nodes.size():
		prefix[i + 1] = prefix[i] + _heights[i] + GAP
	var total := prefix[_nodes.size()] - GAP + PAD_Y
	# 只设高度：宽度回写会改变自身最小尺寸，经 resized 信号再次触发同步。
	_content.custom_minimum_size = Vector2(0.0, maxf(total, 1.0))
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
		row.position = Vector2(col.x, prefix[i])
		row.custom_minimum_size = Vector2(col.y, 0)
		row.size = Vector2(col.y, maxf(_heights[i], 1.0))
	_dbg("sync exit: %d ms" % (Time.get_ticks_msec() - ts))


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
	_heights_dirty = true
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
	_content.custom_minimum_size = Vector2(0.0, maxf(y - GAP + PAD_Y, 1.0))


func _bind_row(row: Control, index: int) -> void:
	if index < 0 or index >= _nodes.size():
		return
	var node: Dictionary = _nodes[index]
	if row.has_method("bind"):
		row.call("bind", node)
	_wire(row, index)
	_heights_dirty = true


func _patch_tool(env: Dictionary) -> void:
	var call_id := _tool_call_id(env)
	if call_id == "":
		_request_sync()
		return
	for i in _nodes.size():
		if str(_nodes[i].get("kind", "")) != "tool":
			continue
		var p: Dictionary = _nodes[i].get("payload", {}) if _nodes[i].get("payload") is Dictionary else {}
		if str(p.get("callId", "")) != call_id:
			continue
		if _live.has(i):
			_bind_row(_live[i], i)
		else:
			_request_sync()
		return
	_request_sync()


func _tool_call_id(env: Dictionary) -> String:
	var data: Dictionary = env.get("data", {}) if env.get("data") is Dictionary else {}
	var id := str(data.get("callId", data.get("id", "")))
	if id != "":
		return id
	var msg: Dictionary = data.get("message", {}) if data.get("message") is Dictionary else {}
	var content: Variant = msg.get("content", [])
	if content is Array:
		for block in content:
			if block is Dictionary and str((block as Dictionary).get("type", "")) == "tool-result":
				return str((block as Dictionary).get("toolCallId", (block as Dictionary).get("id", "")))
	return ""


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
	_heights_dirty = true
	if index >= 0 and index < _nodes.size():
		var row: Control = _live.get(index, null)
		if row != null and row.has_method("is_expanded"):
			var p: Dictionary = _nodes[index].get("payload", {})
			if p is Dictionary:
				p["expanded"] = bool(row.call("is_expanded"))
				_nodes[index]["payload"] = p


func _on_tool_selected(call_id: String, name: String, input: String, output: String) -> void:
	tool_selected.emit(call_id, name, input, output)


func _on_feedback(message_id: String, rating: String, index: int) -> void:
	if index >= 0 and index < _nodes.size():
		var p: Dictionary = _nodes[index].get("payload", {})
		if p is Dictionary:
			p["rating"] = rating
			_nodes[index]["payload"] = p
	feedback_rating.emit(message_id, rating)


# 高度对账在 _process 里做有界收敛（只读缓存的最小尺寸 + 0.5px 迟滞），
# 不再用跨帧 await 协程：协程在 deferred 风暴下会被饿死，且每行一帧的
# 测量节奏让长列表首屏高度长时间停留在估算值。
func _process(_delta: float) -> void:
	if not _built or _live.is_empty() or not _heights_dirty:
		return
	var changed := false
	for idx in _live.keys():
		var i := int(idx)
		if i < 0 or i >= _heights.size():
			continue
		var row: Control = _live[i]
		if row == null or not is_instance_valid(row) or not row.is_inside_tree():
			continue
		var h := row.get_combined_minimum_size().y
		if h < 1.0:
			h = row.size.y
		if h >= 1.0 and absf(h - _heights[i]) > 0.5:
			_heights[i] = h
			changed = true
	if changed:
		_layout_positions()
	else:
		_heights_dirty = false


func _acquire(kind: String) -> Control:
	var pool: Array = _pool.get(kind, [])
	if pool.size() > 0:
		return pool.pop_back()
	var t := Time.get_ticks_msec()
	var scene := _scene_for(kind)
	var row := scene.instantiate()
	_dbg("instantiate %s: %d ms" % [kind, Time.get_ticks_msec() - t])
	return row


func _dbg(m: String) -> void:
	if OS.get_environment("DSHX_UI_DEBUG") != "":
		var line := "[%9.3f f=%d] %s" % [Time.get_ticks_msec() / 1000.0, Engine.get_process_frames(), m]
		print(line)
		var f := FileAccess.open("user://chatlist_log.txt", FileAccess.READ_WRITE)
		if f == null:
			f = FileAccess.open("user://chatlist_log.txt", FileAccess.WRITE)
		if f:
			f.seek_end()
			f.store_line(line)
			f.close()


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
	# 程序化滚动：护栏期内忽略 value_changed，防止「设值→事件→再同步」回环。
	_scroll_programmatic = true
	scroll_vertical = int(_content.custom_minimum_size.y)
	_scroll_programmatic = false
