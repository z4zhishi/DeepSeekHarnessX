extends Tree
class_name SubagentTree

## Parent/child sessions from host/subagent-* events and session parentSession.

signal subagent_selected(id: String)

var _nodes: Dictionary = {}
var _status: Dictionary = {}
var _store: Node = null
var _focused_id: String = ""


func set_store(store: Node) -> void:
	_store = store
	if _focused_id == "" and _store != null and _store.has_method("get_active"):
		var g: Variant = _store.get_active()
		if g is Dictionary:
			_focused_id = str((g as Dictionary).get("id", (g as Dictionary).get("ID", "")))
	_refresh_names()
	_highlight_active_subtree()


func set_sessions(sessions: Array) -> void:
	clear_all()
	var by_id: Dictionary = {}
	for s in sessions:
		if s is Dictionary:
			var d := s as Dictionary
			var id := str(d.get("id", d.get("ID", "")))
			if id != "":
				by_id[id] = d
	var visiting: Dictionary = {}
	for id in by_id.keys():
		_ensure_from_headers(str(id), by_id, visiting)
	_refresh_names()
	_highlight_active_subtree()
	if _focused_id == "" and by_id.size() > 0:
		_focused_id = str(by_id.keys()[0])
		_highlight_active_subtree()


func _ensure_from_headers(id: String, by_id: Dictionary, visiting: Dictionary) -> TreeItem:
	if _nodes.has(id):
		return _nodes[id]
	if visiting.has(id):
		return null
	visiting[id] = true
	var d: Dictionary = by_id.get(id, {"id": id})
	var parent_id := str(d.get("parentSession", d.get("parentSessionId", "")))
	var parent_item: TreeItem = null
	if parent_id != "" and parent_id != id:
		var parent := _ensure_from_headers(parent_id, by_id, visiting)
		if parent == null and _nodes.has(parent_id):
			parent_item = _nodes[parent_id]
		else:
			parent_item = parent
	elif _nodes.has(parent_id):
		parent_item = _nodes[parent_id]
	var item := create_item(parent_item)
	_decorate(item, id, "running")
	_nodes[id] = item
	visiting.erase(id)
	return item


func highlight_session(id: String) -> void:
	_focused_id = id
	_highlight_active_subtree()


func _focused_session_id() -> String:
	if _focused_id != "":
		return _focused_id
	if _store == null or not _store.has_method("get_active"):
		return ""
	var g: Variant = _store.get_active()
	if g is Dictionary:
		return str((g as Dictionary).get("id", (g as Dictionary).get("ID", "")))
	return ""


func active_session_id() -> String:
	return _focused_session_id()


func _highlight_active_subtree() -> void:
	var active := _focused_session_id()
	if active == "":
		return
	if _nodes.has(active):
		var item: TreeItem = _nodes[active]
		item.select(0)
		if has_method("scroll_to_item"):
			scroll_to_item(item)


func _ready() -> void:
	# Apple 化第一批（截图审出缺陷 1/3）：窄侧栏放不下三列表头，收窄最小
	# 列宽并允许横向压缩；Id 列省略显示；去掉表头底色块（浅色主题下是深
	# 灰块，观感突兀）。
	columns = 3
	set_column_title(0, "Agent")
	set_column_title(1, "St")
	set_column_title(2, "Id")
	set_column_titles_visible(true)
	set_column_expand(0, true)
	set_column_expand(1, false)
	set_column_expand(2, false)
	set_column_custom_minimum_width(1, 36)
	set_column_custom_minimum_width(2, 44)
	hide_root = true
	item_selected.connect(_on_item_selected)
	add_theme_color_override("font_color", DshTokens.text_primary())
	add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
	add_theme_stylebox_override("panel", DshTokens.box(Color(0, 0, 0, 0), 0, Color(0, 0, 0, 0), 0, Vector4(0, 0, 0, 0)))
	add_theme_stylebox_override("title_button_normal", DshTokens.box(Color(0, 0, 0, 0), 0, Color(0, 0, 0, 0), 0, Vector4(4, 2, 4, 2)))
	add_theme_stylebox_override("title_button_hover", DshTokens.box(Color(0, 0, 0, 0), 0, Color(0, 0, 0, 0), 0, Vector4(4, 2, 4, 2)))
	add_theme_stylebox_override("title_button_pressed", DshTokens.box(Color(0, 0, 0, 0), 0, Color(0, 0, 0, 0), 0, Vector4(4, 2, 4, 2)))
	add_theme_color_override("title_button_color", DshTokens.text_tertiary())
	add_theme_font_size_override("title_button_font_size", DshTokens.FONT_MICRO)


func _refresh_names() -> void:
	for sid in _nodes.keys():
		var item: TreeItem = _nodes[sid]
		item.set_text(0, _display_name(str(sid)))


func ingest_sessions(sessions: Array) -> void:
	clear_all()
	var by_id: Dictionary = {}
	for s in sessions:
		if s is Dictionary:
			var d := s as Dictionary
			var id := str(d.get("id", d.get("ID", "")))
			if id != "":
				by_id[id] = d
	var visiting: Dictionary = {}
	for id in by_id.keys():
		_ensure_from_headers(str(id), by_id, visiting)
	_refresh_names()
	_highlight_active_subtree()
	_highlight_active_subtree()


func handle_host_event(method: String, payload: Variant) -> void:
	if not (payload is Dictionary):
		return
	var d: Dictionary = payload
	if method == "host/subagent-started":
		_ensure_child(str(d.get("parentSessionId", "")), str(d.get("childSessionId", "")))
		_set_status(str(d.get("childSessionId", "")), "running")
	elif method == "host/subagent-finished":
		var child := str(d.get("childSessionId", ""))
		var st := str(d.get("status", "done"))
		_set_status(child, st, str(d.get("stopReason", "")))
	elif method == "host/session-added":
		var id := str(d.get("id", ""))
		var parent := str(d.get("parentSession", d.get("parentSessionId", "")))
		if parent == "":
			_ensure_root(id)
		else:
			_ensure_child(parent, id)


func add_root_session(session_id: String) -> void:
	_ensure_root(session_id)


func add_subagent(parent_session_id: String, child_session_id: String) -> void:
	_ensure_child(parent_session_id, child_session_id)
	_set_status(child_session_id, "running")


func finish_subagent(child_session_id: String, status: String, stop_reason: String = "") -> void:
	_set_status(child_session_id, status, stop_reason)


func clear_all() -> void:
	clear()
	_nodes.clear()
	_status.clear()


func _ensure_root(session_id: String) -> TreeItem:
	if session_id == "":
		return null
	if _nodes.has(session_id):
		return _nodes[session_id]
	var item := create_item()
	_decorate(item, session_id, "active")
	_nodes[session_id] = item
	return item


func _ensure_child(parent_id: String, child_id: String) -> TreeItem:
	if child_id == "":
		return null
	if _nodes.has(child_id):
		return _nodes[child_id]
	var parent_item: TreeItem = null
	if parent_id != "":
		if _nodes.has(parent_id):
			parent_item = _nodes[parent_id]
		else:
			parent_item = _ensure_root(parent_id)
	var item := create_item(parent_item)
	_decorate(item, child_id, "running")
	_nodes[child_id] = item
	return item


func _decorate(item: TreeItem, session_id: String, status: String) -> void:
	item.set_text(0, _display_name(session_id))
	item.set_metadata(0, session_id)
	item.set_text(2, _short_id(session_id))
	_apply_status(item, status, "")


# 节点名优先用会话标题（store.sessions 镜像里已有），缺失才回退短 id——
# 裸短 id 对用户毫无语义。
func _display_name(session_id: String) -> String:
	var store := _store
	if store == null:
		store = _find_store()
	if store != null and store.has_method("get_session"):
		var s: Dictionary = store.get_session(session_id)
		if not s.is_empty():
			var title := str(s.get("title", ""))
			if title != "":
				return title
	return _short_id(session_id)


func _find_store() -> Node:
	var root := get_tree().get_root()
	if root == null:
		return null
	for child in root.get_children():
		if child.has_method("get_session") and child.has_method("get_sessions"):
			return child
	return null


func _set_status(session_id: String, status: String, stop_reason: String = "") -> void:
	if session_id == "":
		return
	if not _nodes.has(session_id):
		_ensure_root(session_id)
	var item: TreeItem = _nodes[session_id]
	_status[session_id] = status
	_apply_status(item, status, stop_reason)


func _apply_status(item: TreeItem, status: String, stop_reason: String) -> void:
	var st := status.to_lower()
	if st == "ok" or st == "done" or st == "finished":
		item.set_text(1, "done")
		item.set_custom_color(1, DshTokens.success())
	elif st == "error" or st == "failed":
		item.set_text(1, "error")
		item.set_custom_color(1, DshTokens.danger())
	elif st == "running":
		item.set_text(1, "running")
		item.set_custom_color(1, DshTokens.warn())
	else:
		item.set_text(1, "active")
		item.set_custom_color(1, DshTokens.success())
	if stop_reason != "":
		item.set_tooltip_text(0, stop_reason)


func _short_id(id: String) -> String:
	return id.substr(0, 8) if id.length() > 8 else id


func _on_item_selected() -> void:
	var item := get_selected()
	if item == null:
		return
	var sid := str(item.get_metadata(0))
	if sid != "":
		subagent_selected.emit(sid)
