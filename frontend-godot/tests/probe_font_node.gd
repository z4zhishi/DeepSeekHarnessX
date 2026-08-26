extends Node
## SystemFont 嫌疑对照：同一句 "Hello! How can I help you today?"
## A=默认内置字体  B=主题同款 SystemFont 名字链。各测首帧整形耗时。
var _t0 := 0
func _stamp(m: String) -> void:
	print("%8.3fs %s" % [(Time.get_ticks_msec() - _t0) / 1000.0, m])
func _ready() -> void:
	_t0 = Time.get_ticks_msec()
	_run()
func _run() -> void:
	var mk := func(names: PackedStringArray) -> RichTextLabel:
		var rl := RichTextLabel.new()
		rl.bbcode_enabled = true
		rl.fit_content = true
		rl.text = "Hello! How can I help you today? 芝士的妙妙自部署模型"
		if names.size() > 0:
			var f := SystemFont.new()
			f.font_names = names
			rl.add_theme_font_override("normal_font", f)
		add_child(rl)
		return rl
	var a: RichTextLabel = mk.call(PackedStringArray())
	await get_tree().process_frame
	_stamp("A default-font first frame OK")
	a.queue_free()
	await get_tree().process_frame
	var t := Time.get_ticks_msec()
	var b: RichTextLabel = mk.call(PackedStringArray(["Segoe UI", "PingFang SC", "Microsoft YaHei", "Noto Sans CJK SC", "sans-serif"]))
	await get_tree().process_frame
	_stamp("B systemfont first frame: %d ms" % (Time.get_ticks_msec() - t))
	b.queue_free()
	await get_tree().process_frame
	_stamp("FONT_PROBE_DONE")
	get_tree().quit(0)
