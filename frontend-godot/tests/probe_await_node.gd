extends Node
## 最小 await 复现：A=直接链式 await；B=fire-and-forget 启动的 while+await 循环。
var _t0 := 0
func _stamp(m: String) -> void:
	var f := FileAccess.open("user://await_log.txt", FileAccess.READ_WRITE)
	f.seek_end()
	f.store_line("%8.3fs %s" % [(Time.get_ticks_msec() - _t0) / 1000.0, m])
	f.close()
func _ready() -> void:
	_t0 = Time.get_ticks_msec()
	var f := FileAccess.open("user://await_log.txt", FileAccess.WRITE)
	f.store_line("=== await probe ===")
	f.close()
	_run()
	_bg_loop()
func _run() -> void:
	_stamp("chain: before")
	await get_tree().process_frame
	_stamp("chain: resume1 OK")
	await get_tree().process_frame
	_stamp("chain: resume2 OK")
func _bg_loop() -> void:
	var q := [1, 2, 3]
	while q.size() > 0:
		var i = q.pop_front()
		await get_tree().process_frame
		_stamp("bg resumed i=%d" % i)
	_stamp("BG_LOOP_DONE")
