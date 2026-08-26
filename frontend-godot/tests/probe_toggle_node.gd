extends Node
## 侧栏收起冻结复现探针：实例化真实主场景，程序化反复切换侧栏，
## 监视帧计数是否推进。全部日志写 user://toggle_log.txt。
## 运行：godot --headless --path . res://tests/probe_toggle.tscn

var _t0 := 0
var _frames0 := 0

func _stamp(m: String) -> void:
	var line := "%8.3fs f=%d %s" % [(Time.get_ticks_msec() - _t0) / 1000.0, Engine.get_process_frames(), m]
	var f := FileAccess.open("user://toggle_log.txt", FileAccess.READ_WRITE)
	if f == null:
		f = FileAccess.open("user://toggle_log.txt", FileAccess.WRITE)
	if f:
		f.seek_end()
		f.store_line(line)
		f.close()

func _ready() -> void:
	_t0 = Time.get_ticks_msec()
	var f := FileAccess.open("user://toggle_log.txt", FileAccess.WRITE)
	f.store_line("=== toggle probe start ===")
	f.close()
	_run()

func _run() -> void:
	var scene: PackedScene = load("res://scenes/app.tscn")
	var app = scene.instantiate()
	add_child(app)
	for i in 10:
		await get_tree().process_frame
	_stamp("app ready")
	_frames0 = Engine.get_process_frames()

	# 找到 app 根脚本（app.gd 挂在场景根）
	var root_script = app
	if not root_script.has_method("_apply_columns"):
		_stamp("ERROR: app script without _apply_columns")
		get_tree().quit(1)
		return

	# 反复切换侧栏 30 次，每两次之间等 5 帧
	for round_i in 30:
		var t := Time.get_ticks_msec()
		root_script._toggle_sidebar()
		await get_tree().process_frame
		await get_tree().process_frame
		await get_tree().process_frame
		await get_tree().process_frame
		await get_tree().process_frame
		var dt := Time.get_ticks_msec() - t
		var df := Engine.get_process_frames() - _frames0
		if dt > 100 or df < (round_i + 1) * 5:
			_stamp("round %d SLOW: %d ms, frames advanced=%d" % [round_i, dt, df])
	_frames0 = Engine.get_process_frames()
	_stamp("TOGGLE_DONE total_frames=%d" % _frames0)
	get_tree().quit(0)
