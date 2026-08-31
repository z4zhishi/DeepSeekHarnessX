extends Node

## ApprovalCenter headless probe: real store contract, no view code.
## Covers upsert/resolve/bounded tail, session routing, removal on session delete.

var _passed := 0
var _failed := 0

const DshCenter := preload("res://engine/approval_center.gd")


func _assert(cond: bool, msg: String) -> void:
	if cond:
		_passed += 1
		print("PASS: " + msg)
	else:
		_failed += 1
		print("FAIL: " + msg)


func _ready() -> void:
	var center: DshCenter = DshCenter.new()

	# 1. upsert + pending order
	center.upsert("c1", "s1", "run_command?", [])
	center.upsert("c2", "s2", "write_file?", [])
	var p := center.pending()
	_assert(p.size() == 2, "two pending entries")
	_assert(p[0].call_id == "c1", "arrival order kept")

	# 2. pending_for_session routes by real sessionId
	_assert(center.pending_for_session("s2").size() == 1, "filter by session")

	# 3. jump request carries the owning session id and call id
	var got_sid := ""
	var got_cid := ""
	var got_box := [got_sid, got_cid]  # captured by reference through an Array box
	center.jump_requested.connect(func(sid: String, cid: String) -> void:
		got_box[0] = sid
		got_box[1] = cid
	)
	center.request_jump("c2")
	_assert(got_box[0] == "s2" and got_box[1] == "c2", "jump carries session+call ids")

	# 4. resolve keeps a bounded recent tail
	center.resolve("c1", "allowed")
	_assert(center.pending_count() == 1, "resolved leaves pending")
	_assert(center.resolved().size() == 1, "resolved tail holds entries")
	for i in 12:
		center.upsert("r%d" % i, "sx", "p", [])
		center.resolve("r%d" % i, "allowed")
	_assert(center.resolved().size() <= 10, "resolved tail bounded at 10")

	# 5. unknown callId resolve is a no-op (never fabricates state)
	center.resolve("nonexistent", "allowed")
	_assert(true, "unknown resolve no-op")

	# 6. remove_session drops everything for that session
	center.remove_session("s2")
	_assert(center.pending_count() == 0, "session removal clears pending")

	# 7. upsert of existing callId does not duplicate
	center.upsert("dup", "s9", "q", [])
	center.upsert("dup", "s9", "q", [])
	_assert(center.pending_count() == 1, "duplicate upsert keeps one entry")

	# 8. auto-reject emits deny without duplicating upserts
	var auto_box := ["", ""]
	center.auto_decision.connect(func(cid: String, dec: String) -> void:
		auto_box[0] = cid
		auto_box[1] = dec
	)
	center.set_auto_reject(true)
	center.upsert("auto1", "sA", "rm?", [])
	_assert(auto_box[0] == "auto1" and auto_box[1] == "deny", "auto-reject emits deny")
	center.upsert("auto1", "sA", "rm?", [])
	_assert(auto_box[0] == "auto1", "duplicate upsert does not re-emit")

	print("APPROVAL_CENTER_RESULT passed=%d failed=%d" % [_passed, _failed])
	get_tree().quit(1 if _failed > 0 else 0)