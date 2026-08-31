extends Node

## GUI structure probe: validates tab switching, sidebar cwd grouping,
## and lineage highlight. No visual asserts — purely structural/state checks.

var _passed := 0
var _failed := 0

const SIZES := [
	Vector2i(1440, 900),
	Vector2i(1024, 640),
	Vector2i(960, 600),
]


func _ready() -> void:
	var scene: PackedScene = load("res://scenes/app.tscn")
	if scene == null:
		print("GUI_STRUCTURE_RESULT passed=0 failed=1 reason=scene-load")
		get_tree().quit(1)
		return
	var app: Control = scene.instantiate()
	add_child(app)
	await _frames(12)

	_seed_store(app)

	for size in SIZES:
		DisplayServer.window_set_size(size)
		app.get_viewport().size = size
		await _frames(8)
		app._apply_columns()
		await _frames(8)
		var label := "%dx%d" % [size.x, size.y]
		_verify_tabs(app, label)
		_verify_sidebar_cwd_grouping(app, label)
		# roundtrip 是协程：必须 await，否则结果在协程完成前被打印且 quit
		# 与未完成协程竞争（曾挂住 Godot 主循环）。
		await _verify_session_pick_roundtrip(app, label)
		print("GUI_STRUCTURE_PROBE %s passed=%d failed=%d" % [label, _passed, _failed])

	print("GUI_STRUCTURE_RESULT passed=%d failed=%d" % [_passed, _failed])
	get_tree().quit(1 if _failed > 0 else 0)


func _seed_store(app: Control) -> void:
	var store: Node = app._store
	if store == null or not store.has_method("set_sessions"):
		return
	store.set_sessions([
		{"id": "s1", "title": "Alpha session", "cwd": "C:/repo"},
		{"id": "s2", "title": "Beta session", "cwd": "C:/repo"},
		{"id": "s3", "title": "Gamma session", "cwd": "D:/workspace"},
	])
	store.set_active("s2")


func _verify_tabs(app: Control, label: String) -> void:
	# Switch to chat
	_emit_tab(app, "chat")
	_assert(app._chat_tab.visible, "%s chat tab visible" % label)
	# Switch to trajectory
	_emit_tab(app, "trajectory")
	_assert(app._chat_tab.visible == false, "%s chat hidden on trajectory" % label)
	if app._traj_view != null:
		_assert(app._traj_view.visible, "%s trajectory view visible" % label)
	if app._lineage != null:
		_assert(app._lineage.visible == false, "%s lineage hidden on trajectory" % label)
	# Switch to lineage
	_emit_tab(app, "lineage")
	_assert(app._chat_tab.visible == false, "%s chat hidden on lineage" % label)
	if app._lineage != null:
		_assert(app._lineage.visible, "%s lineage tree visible" % label)


func _verify_sidebar_cwd_grouping(app: Control, label: String) -> void:
	var sidebar: Node = app._sidebar
	if sidebar == null or sidebar._list == null:
		return
	var list: ItemList = sidebar._list
	var groups := 0
	var sessions := 0
	for i in list.item_count:
		var meta: Variant = list.get_item_metadata(i)
		if meta is Dictionary:
			if str(meta.get("kind", "")) == "cwd_header":
				groups += 1
				_assert(not list.is_item_selectable(i), "%s cwd header %d not selectable" % [label, i])
			elif str(meta.get("kind", "")) == "session":
				sessions += 1
	_assert(groups >= 2, "%s at least two cwd groups visible" % label)
	_assert(sessions >= 3, "%s all seed sessions rendered" % label)


func _verify_session_pick_roundtrip(app: Control, label: String) -> void:
	_emit_tab(app, "chat")
	await _frames(4)
	# Pick first selectable session from sidebar list (skip cwd headers).
	var sidebar: Node = app._sidebar
	if sidebar == null or sidebar._list == null:
		return
	var list: ItemList = sidebar._list
	for i in list.item_count:
		if not list.is_item_selectable(i):
			continue
		var meta: Variant = list.get_item_metadata(i)
		if meta is Dictionary and str(meta.get("kind", "")) == "session":
			var id := str(meta.get("id", ""))
			if id == "":
				continue
			list.select(i)
			sidebar._on_item_selected(i)
			await _frames(8)
			_assert(app._active_id() == id, "%s sidebar pick switches active session to %s" % [label, id])
			_assert(app._header._title.text.strip_edges() != "", "%s header title updates after pick" % label)
			_emit_tab(app, "lineage")
			await _frames(6)
			if app._lineage != null:
				_assert(app._lineage.active_session_id() == id, "%s lineage active id reflects picked session" % label)
			_emit_tab(app, "trajectory")
			await _frames(6)
			_assert(app._traj.visible, "%s trajectory visible after roundtrip" % label)
			_emit_tab(app, "chat")
			await _frames(6)
			_assert(app._chat_tab.visible, "%s chat visible after roundtrip" % label)
			return
	# 没有可选会话行（异常）：记一次失败而非静默通过。
	_assert(false, "%s no selectable session row found" % label)


func _emit_tab(app: Control, name: String) -> void:
	if app._header.has_signal("tab_changed"):
		app._header.tab_changed.emit(name)
		return
	app._on_tab(name)


func _assert(cond: bool, msg: String) -> void:
	if cond:
		_passed += 1
		print("  [PASS] %s" % msg)
	else:
		_failed += 1
		print("  [FAIL] %s" % msg)


func _frames(n: int) -> void:
	for _i in n:
		await get_tree().process_frame