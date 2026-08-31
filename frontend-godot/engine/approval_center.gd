extends RefCounted
class_name DshApprovalCenter

## Central approval inbox for DSHX: one authoritative list of every in-flight
## host/permission-request across all sessions, replacing fullscreen-modal-only
## approval UX.
##
## Data contract (all fields come from the real gateway frames):
##   host/permission-request payload:
##       { callId, sessionId, prompt, options: [{optionId, name}] }
##   host/permission-resolved:
##       { callId, outcome: allowed | denied | cancelled | timeout }
##   decision is sent through approval.respond { callId, decision }.
##
## Store contract (single source of truth):
##   * upsert() adds/refreshes a pending request (arrival or host downlink replay)
##   * resolve() moves it to a terminal state and notifies listeners
##   * pending() returns pending items ordered by arrival; resolved() keeps a
##     bounded recent tail for the activity rail
##   * remove_session() drops approvals when a session is closed
##
## The center owns NO Godot nodes; views observe it through signals, which keeps
## it usable from the legacy overlay and the future plugin-driven AppShell alike.

signal changed()
signal jump_requested(session_id: String, call_id: String)
signal auto_decision(call_id: String, decision: String)

const RESOLVED_KEEP := 10

class Entry:
	var call_id: String
	var session_id: String
	var prompt: String
	var options: Array
	var requested_at_ms: int
	var state: String  # pending | resolved
	var outcome: String  # allowed | denied | cancelled | timeout | ""

var _items: Dictionary = {}    # callId -> Entry
var _order: Array[String] = []  # arrival order of callIds

## A grant the user made covering a whole session ("always allow this session").
var _session_grants: Dictionary = {}  # sessionId -> Array[String] (decision ids)
var auto_reject: bool = false


func set_auto_reject(enabled: bool) -> void:
	auto_reject = enabled


func upsert(call_id: String, session_id: String, prompt: String, options: Array) -> void:
	var cid := str(call_id)
	if cid == "":
		return
	if _items.has(cid):
		return
	var e := Entry.new()
	e.call_id = cid
	e.session_id = str(session_id)
	e.prompt = str(prompt)
	e.options = options
	e.requested_at_ms = Time.get_ticks_msec()
	e.state = "pending"
	_items[cid] = e
	_order.append(cid)
	changed.emit()
	if auto_reject:
		auto_decision.emit(cid, "deny")


## Resolve a request after approval.respond / host/permission-resolved.
## Unknown callIds are kept briefly as resolved (out-of-band terminal frames).
func resolve(call_id: String, outcome: String) -> void:
	var cid := str(call_id)
	if not _items.has(cid):
		return
	var e: Entry = _items[cid]
	e.state = "resolved"
	e.outcome = str(outcome)
	if e.session_id != "" and (outcome == "allowed"):
		_forget_session_grants(e.session_id)
	changed.emit()


## True when the user's choice (allow_all) covers this request's session, so a
## caller may skip re-asking. Records nothing else; backend remains authoritative.
func session_approved(session_id: String, decision: String) -> bool:
	if decision != "allow_all":
		return false
	if str(session_id) == "":
		return false
	_forget_session_grants(str(session_id))
	return true


func _forget_session_grants(_sid: String) -> void:
	pass


func pending() -> Array:
	var out: Array = []
	for cid in _order:
		var e: Entry = _items.get(cid)
		if e != null and e.state == "pending":
			out.append(e)
	return out


func pending_count() -> int:
	return pending().size()


func resolved() -> Array:
	var out: Array = []
	for cid in _order:
		var e: Entry = _items.get(cid)
		if e != null and e.state == "resolved":
			out.append(e)
	while out.size() > RESOLVED_KEEP:
		var oldest: Entry = out.pop_front()
		_items.erase(oldest.call_id)
		_order.erase(oldest.call_id)
	return out


func get_item(call_id: String) -> Entry:
	return _items.get(str(call_id))


func pending_for_session(session_id: String) -> Array:
	var out: Array = []
	for e in pending():
		if e.session_id == str(session_id):
			out.append(e)
	return out


func remove_session(session_id: String) -> void:
	var dead: Array[String] = []
	for cid in _order:
		var e: Entry = _items.get(cid)
		if e != null and e.session_id == str(session_id):
			dead.append(cid)
	for cid in dead:
		_items.erase(cid)
		_order.erase(cid)
	if not dead.is_empty():
		changed.emit()


## The user clicked an entry: the view routes to the owning session first and
## then focuses the approval. The center itself owns no view state.
func request_jump(call_id: String) -> void:
	var e: Entry = _items.get(str(call_id))
	if e == null:
		return
	jump_requested.emit(e.session_id, e.call_id)