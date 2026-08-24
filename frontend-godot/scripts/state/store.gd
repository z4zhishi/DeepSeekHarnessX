extends Node
class_name SessionStore

## In-memory session list and parent/child lineage. No fake local delete/rename.

signal sessions_changed
signal active_session_changed(id: String)
signal lineage_changed(parts: Array)

var sessions: Array = []
var active_id: String = ""
var _parent: Dictionary = {}


func set_sessions(list: Array) -> void:
	sessions = []
	_parent.clear()
	for item in list:
		if item is Dictionary:
			_upsert(item as Dictionary, false)
	sessions_changed.emit()
	if active_id != "":
		lineage_changed.emit(_lineage_parts(active_id))


func upsert_session(header: Dictionary) -> void:
	_upsert(header, true)


func _upsert(header: Dictionary, emit_change: bool) -> void:
	var id := _header_id(header)
	if id == "":
		return
	var found := false
	for i in sessions.size():
		if _header_id(sessions[i]) == id:
			var merged: Dictionary = {}
			if sessions[i] is Dictionary:
				merged = (sessions[i] as Dictionary).duplicate()
			merged.merge(header, true)
			sessions[i] = merged
			found = true
			break
	if not found:
		sessions.append(header)
	var parent := _header_parent(header)
	if parent != "":
		set_parent(id, parent)
	if emit_change:
		sessions_changed.emit()
		if id == active_id:
			lineage_changed.emit(_lineage_parts(active_id))


func set_active(session_id: String) -> void:
	active_id = session_id
	active_session_changed.emit(session_id)
	lineage_changed.emit(_lineage_parts(session_id))


func get_active() -> Dictionary:
	return get_session(active_id)


func get_sessions() -> Array:
	return sessions


func get_session(session_id: String) -> Dictionary:
	for s in sessions:
		if s is Dictionary and _header_id(s) == session_id:
			return s
	return {}


func set_parent(child_id: String, parent_id: String) -> void:
	if child_id == "" or parent_id == "" or child_id == parent_id:
		return
	_parent[child_id] = parent_id
	if get_session(parent_id).is_empty():
		_upsert({"id": parent_id}, false)


func children_of(session_id: String) -> Array:
	var out: Array = []
	for c in _parent.keys():
		if str(_parent[c]) == session_id:
			out.append(str(c))
	return out


func breadcrumb(session_id: String) -> Array:
	var chain: Array = []
	var cur := session_id
	var guard := 0
	while cur != "" and guard < 64:
		chain.push_front(cur)
		if _parent.has(cur) and str(_parent[cur]) != "":
			cur = str(_parent[cur])
		else:
			cur = ""
		guard += 1
	return chain


func _lineage_parts(session_id: String) -> Array:
	var parts: Array = []
	for sid in breadcrumb(session_id):
		var s := get_session(str(sid))
		var title := str(s.get("title", "")) if not s.is_empty() else ""
		if title == "":
			title = str(sid)
		parts.append({"id": str(sid), "title": title})
	return parts


func _header_id(header: Dictionary) -> String:
	var id := str(header.get("id", ""))
	if id == "":
		id = str(header.get("ID", ""))
	return id


func _header_parent(header: Dictionary) -> String:
	var p := str(header.get("parentSession", ""))
	if p == "":
		p = str(header.get("parentSessionId", ""))
	return p
