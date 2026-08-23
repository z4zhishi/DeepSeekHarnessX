extends Node
class_name SessionStore

## 真会话列表（波3）：以 store 为唯一数据源，侧栏/面包屑/谱系均由此驱动。
## 会话元数据 {id, title, preset, cwd, createdAt, status} 由后端 session.list 归置；
## host/session-added 事件也经 upsert_session 进入同一列表，保证谱系不悬空。
## 额外维护父子谱系（child_id -> parent_id）与当前会话祖先链（breadcrumb）。

signal sessions_changed(sessions: Array)
signal session_added(session_id: String)
signal active_session_changed(session_id: String)
signal lineage_changed(session_id: String)   # 当前会话祖先链变化（面包屑刷新）
signal message_appended(role: String, text: String)
signal event_buffered(env: Dictionary)

const MAX_EVENTS_PER_SESSION := 4096

var sessions: Array[Dictionary] = []
var active_session_id: String = ""
var _events: Dictionary = {}      # session_id -> Array[Dictionary]
var _parent: Dictionary = {}     # child_id -> parent_id
var _lineage: Array[String] = [] # 当前会话祖先链（root..active）

## 归置一个会话头：已存在则覆盖，否则追加并广播 session_added。
func upsert_session(header: Dictionary) -> void:
	var id: String = str(header.get("id", ""))
	if id == "":
		return
	for i in sessions.size():
		if str(sessions[i].get("id", "")) == id:
			var merged := {}
			merged.merge(header)
			sessions[i] = merged
			sessions_changed.emit(sessions)
			return
	sessions.append(header)
	session_added.emit(id)
	sessions_changed.emit(sessions)

func get_session(session_id: String) -> Dictionary:
	for s in sessions:
		if str(s.get("id", "")) == session_id:
			return s
	return {}

## 建立父子谱系（host/session-added 携带 parentSessionId 时归位）。
func set_parent(child_id: String, parent_id: String) -> void:
	if child_id == "" or parent_id == "" or child_id == parent_id:
		return
	_parent[child_id] = parent_id
	# 父会话若未登记，补一个建档，避免谱系根悬空
	if get_session(parent_id).is_empty():
		upsert_session({"id": parent_id})

func is_root(session_id: String) -> bool:
	return not _parent.has(session_id) or str(_parent[session_id]) == ""

func children_of(session_id: String) -> Array:
	var out: Array = []
	for c in _parent.keys():
		if str(_parent[c]) == session_id:
			out.append(c)
	return out

## 当前会话祖先链（root..session），供面包屑逐级导航。
func lineage() -> Array[String]:
	return _lineage

## 由任意会话向上回溯祖先链。
func breadcrumb(session_id: String) -> Array[String]:
	var chain: Array[String] = []
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

func set_active(session_id: String) -> void:
	active_session_id = session_id
	_lineage = breadcrumb(session_id)
	active_session_changed.emit(session_id)
	lineage_changed.emit(session_id)

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
	_parent.clear()
	_lineage.clear()
	active_session_id = ""
