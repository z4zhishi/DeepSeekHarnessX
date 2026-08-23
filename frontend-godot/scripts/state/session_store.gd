extends Node
class_name SessionStore

signal sessions_changed(sessions: Array)
signal active_session_changed(session_id: String)
signal message_appended(role: String, text: String)
signal event_buffered(env: Dictionary)

const MAX_EVENTS_PER_SESSION := 4096

var sessions: Array[Dictionary] = []
var active_session_id: String = ""
var _events: Dictionary = {}  # session_id -> Array[Dictionary]

func upsert_session(header: Dictionary) -> void:
	for i in sessions.size():
		if sessions[i].get("id", "") == header.get("id", ""):
			sessions[i] = header
			sessions_changed.emit(sessions)
			return
	sessions.append(header)
	sessions_changed.emit(sessions)

func set_active(session_id: String) -> void:
	active_session_id = session_id
	active_session_changed.emit(session_id)

func events_for(session_id: String) -> Array:
	return _events.get(session_id, [])

func append_event(env: Dictionary) -> void:
	var sid = active_session_id
	var list: Array = _events.get(sid, [])
	list.append(env)
	if list.size() > MAX_EVENTS_PER_SESSION:
		list.pop_front()
	_events[sid] = list
	event_buffered.emit(env)

func clear() -> void:
	sessions.clear()
	_events.clear()
	active_session_id = ""