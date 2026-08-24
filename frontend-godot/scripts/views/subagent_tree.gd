extends Tree
class_name SubagentTree

## Parent/child sessions from host/subagent-* events and session parentSession.

signal subagent_selected(id: String)

var _nodes: Dictionary = {}
var _status: Dictionary = {}


func _ready() -> void:
	columns = 3
	set_column_title(0, "Agent")
	set_column_title(1, "Status")
	set_column_title(2, "Id")
	set_column_titles_visible(true)
	set_column_expand(0, true)
	set_column_expand(1, false)
	set_column_expand(2, false)
	set_column_custom_minimum_width(1, 80)
	set_column_custom_minimum_width(2, 88)
	hide_root = true
	item_selected.connect(_on_item_selected)
	add_theme_color_override("font_color", DshTokens.text_primary())
	add_theme_stylebox_override("panel", DshTokens.box(Color(0, 0, 0, 0), 0, Color(0, 0, 0, 0), 0, Vector4(0, 0, 0, 0)))


func ingest_sessions(sessions: Array) -> void:
	for s in sessions:
		if not (s is Dictionary):
			continue
		var d: Dictionary = s
		var id := str(d.get("id", d.get("ID", "")))
		if id == "":
			continue
		var parent := str(d.get("parentSession", d.get("parentSessionId", "")))
		if parent == "":
			_ensure_root(id)
		else:
			_ensure_child(parent, id)


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
	item.set_text(0, _short_id(session_id))
	item.set_metadata(0, session_id)
	item.set_text(2, _short_id(session_id))
	_apply_status(item, status, "")


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
