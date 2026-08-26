extends SceneTree
## DSHX 渲染压力探针 v2：分阶段把时间戳写进 user://stress_log.txt（stdout 有缓冲不可靠）。
## 用法：godot --path . --rendering-method <m> --rendering-driver <d> -s res://tests/render_stress.gd
## 60 个富文本行、30 帧后退出。崩溃=signal 11；卡顿=log 时间戳暴露在哪个阶段。

var _t0 := 0

func _stamp(msg: String) -> void:
	var f := FileAccess.open("user://stress_log.txt", FileAccess.READ_WRITE)
	f.seek_end()
	f.store_line("%8.3fs %s" % [(Time.get_ticks_msec() - _t0) / 1000.0, msg])
	f.close()

func _initialize() -> void:
	_t0 = Time.get_ticks_msec()
	var f := FileAccess.open("user://stress_log.txt", FileAccess.WRITE)
	f.store_line("=== run start ===")
	f.close()
	_run()

func _run() -> void:
	_stamp("adapter=" + str(RenderingServer.get_video_adapter_name()) + " api=" + str(RenderingServer.get_current_rendering_method()))
	var root_ctl := Control.new()
	root_ctl.set_anchors_preset(Control.PRESET_FULL_RECT)
	root.add_child(root_ctl)
	var scroll := ScrollContainer.new()
	scroll.set_anchors_preset(Control.PRESET_FULL_RECT)
	root_ctl.add_child(scroll)
	var box := VBoxContainer.new()
	box.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	scroll.add_child(box)
	var chunks := [
		"分析完成：目标函数 _process 位于第 %d 行，复杂度 O(n·log n)。",
		"The quick brown fox 🦊 jumps over 12 lazy dogs — 混合文本 0x1F98A。",
		"【工具调用】terminal.exec → exit code 0，耗时 128ms，输出已折叠。",
	]
	for i in 60:
		var rl := RichTextLabel.new()
		rl.bbcode_enabled = true
		rl.fit_content = true
		rl.custom_minimum_size = Vector2(0, 28)
		rl.text = "[b][color=#7aa2f7]turn%d[/color][/b] " % i + chunks[i % chunks.size()]
		box.add_child(rl)
		if i % 20 == 19:
			await process_frame
			_stamp("built %d labels" % (i + 1))
	_stamp("build done, scrolling frames")
	for f2 in 30:
		scroll.scroll_vertical = f2 * 40
		await process_frame
	_stamp("STRESS_DONE_OK")
	quit(0)
