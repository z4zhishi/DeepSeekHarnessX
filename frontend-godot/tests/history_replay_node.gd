extends Node
## DSHX 历史重放试验台（场景版，autoload 完整加载）：
## session.list → fetch_history(0) → ConversationFold.ingest_history →
## ChatList.set_nodes / TrajectoryView.set_events，逐会话推进。
## 运行：godot --path . res://tests/history_replay.tscn [--headless]

const BASE_URL := "http://127.0.0.1:3199"

var _t0 := 0
var _client: DshClient
var _chat: ChatList
var _traj: TrajectoryView

func _stamp(msg: String) -> void:
	var line := "%8.3fs %s" % [(Time.get_ticks_msec() - _t0) / 1000.0, msg]
	print(line)
	var f := FileAccess.open("user://replay_log.txt", FileAccess.READ_WRITE)
	f.seek_end()
	f.store_line(line)
	f.close()

func _ready() -> void:
	_t0 = Time.get_ticks_msec()
	var f := FileAccess.open("user://replay_log.txt", FileAccess.WRITE)
	f.store_line("=== replay run start ===")
	f.close()
	_run()

func _run() -> void:
	_client = DshClient.new()
	_client.base_url = BASE_URL
	add_child(_client)
	_chat = ChatList.new()
	_chat.size = Vector2(900, 600)
	add_child(_chat)
	_traj = TrajectoryView.new()
	add_child(_traj)
	await get_tree().process_frame

	var ids: Array = []
	var listed := {"done": false}
	_client.list_sessions(func(ok: bool, data: Variant) -> void:
		if ok and data is Array:
			for h in data:
				var id := ""
				if h is Dictionary:
					id = str(h.get("id", ""))
				elif h is String:
					id = str(h)
				if id != "":
					ids.append(id)
		listed.done = true
	)
	for i in 240:
		await get_tree().process_frame
		if bool(listed.done):
			break
	_stamp("sessions listed: " + str(ids))
	for id in ids:
		await _replay_one(str(id))
	_stamp("ALL_DONE_OK")
	get_tree().quit(0)

func _replay_one(id: String) -> void:
	var got := {"done": false, "ok": false, "data": null}
	_client.fetch_history(id, 0, func(ok: bool, data: Variant) -> void:
		got.ok = ok
		got.data = data
		got.done = true
	)
	for i in 240:
		await get_tree().process_frame
		if bool(got.done):
			break
	if not bool(got.done):
		_stamp("session %s: TIMEOUT waiting history" % id)
		return
	var events: Array = []
	if got.data is Array:
		events = got.data
	elif got.data is Dictionary:
		var ev = got.data.get("events", [])
		if ev is Array:
			events = ev
	_stamp("session %s: %d events" % [id, events.size()])
	var fold := ConversationFold.new()
	fold.ingest_history(events)
	var nodes := fold.nodes()
	var summary := []
	for n in nodes:
		var p = n.get("payload", {})
		var tl := 0
		if p is Dictionary:
			tl = str(p.get("text", "")).length() + str(p.get("output", "")).length()
		summary.append("%s(len=%d)" % [str(n.get("kind", "?")), tl])
	_stamp("  folded -> %d nodes: %s" % [nodes.size(), ", ".join(summary)])
	if _traj.has_method("set_events"):
		var t1 := Time.get_ticks_msec()
		_traj.set_events(events)
		_stamp("  traj.set_events took %d ms" % (Time.get_ticks_msec() - t1))
	var t2 := Time.get_ticks_msec()
	_chat.set_nodes(nodes)
	_stamp("  chat.set_nodes took %d ms" % (Time.get_ticks_msec() - t2))
	for i in 10:
		var tf := Time.get_ticks_msec()
		await get_tree().process_frame
		var dt := Time.get_ticks_msec() - tf
		if dt > 50:
			_stamp("  frame %d SLOW: %d ms" % [i, dt])
	_stamp("  rendered ok")
