extends Node
## 崩溃帧二分探针 v2：全部打点写 user://probe_log.txt（stdout 缓冲不可靠）。
## 运行：godot --headless --path . res://tests/probe_frame.tscn

var _t0 := 0

func _stamp(m: String) -> void:
	var line := "%8.3fs f=%d %s" % [(Time.get_ticks_msec() - _t0) / 1000.0, Engine.get_process_frames(), m]
	var f := FileAccess.open("user://probe_log.txt", FileAccess.READ_WRITE)
	f.seek_end()
	f.store_line(line)
	f.close()

func _ready() -> void:
	_t0 = Time.get_ticks_msec()
	var f := FileAccess.open("user://probe_log.txt", FileAccess.WRITE)
	f.store_line("=== probe start ===")
	f.close()
	_run()

func _run() -> void:
	var txt := FileAccess.get_file_as_string("I:/KH/Teplix/DSHX/backend/dumped_session.json")
	var events: Array = JSON.parse_string(txt)
	if events == null or events.is_empty():
		_stamp("FAILED to load dumped_session.json")
		get_tree().quit(1)
		return
	_stamp("loaded %d events" % events.size())
	var fold := ConversationFold.new()
	fold.ingest_history(events)
	var all := fold.nodes()
	_stamp("folded %d nodes" % all.size())

	var chat := ChatList.new()
	chat.size = Vector2(900, 600)
	add_child(chat)
	await get_tree().process_frame
	_stamp("chat ready")

	var t := Time.get_ticks_msec()
	chat.set_nodes(all)
	await get_tree().process_frame
	await get_tree().process_frame
	_stamp("PASS1 full: %d ms" % (Time.get_ticks_msec() - t))

	t = Time.get_ticks_msec()
	chat.set_nodes(all)
	await get_tree().process_frame
	await get_tree().process_frame
	_stamp("PASS2 full repeat: %d ms" % (Time.get_ticks_msec() - t))

	for kind in ["user", "turn-error", "assistant"]:
		var sub := []
		for n in all:
			if str(n.get("kind", "")) == kind:
				sub.append(n)
		if sub.is_empty():
			continue
		t = Time.get_ticks_msec()
		chat.set_nodes(sub)
		await get_tree().process_frame
		await get_tree().process_frame
		_stamp("subset %-12s (%d): %d ms" % [kind, sub.size(), Time.get_ticks_msec() - t])
	_stamp("PROBE_DONE")
	get_tree().quit(0)
