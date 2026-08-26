extends Node
## FilePicker 探针：实例化组件 → 地址栏导航 → 驱动真实选择（OK 按钮）→
## 信号断言 → 最近记录持久化断言 → 键盘行为（BackSpace 上级 / Ctrl+L 聚焦地址）。
## 全部走公共 API，不依赖对话框内部控件（headless 下内部控件懒构建不可见）。
## 运行：godot --headless --path . res://tests/probe_filepicker.tscn
## 日志：user://filepicker_log.txt；失败以非零码退出。

var _fails := 0
## 成员变量而非局部：GDScript lambda 按值捕获局部变量，成员经 self 引用可写。
var _got_dir := ""


func _check(name: String, ok: bool, detail := "") -> void:
	var line := "%s %s%s" % ["PASS" if ok else "FAIL", name, ("  [%s]" % detail) if detail != "" else ""]
	print(line)
	_stamp(line)
	if not ok:
		_fails += 1


func _stamp(m: String) -> void:
	var f := FileAccess.open("user://filepicker_log.txt", FileAccess.READ_WRITE)
	if f == null:
		f = FileAccess.open("user://filepicker_log.txt", FileAccess.WRITE)
	if f:
		f.seek_end()
		f.store_line(m)
		f.close()


func _ready() -> void:
	var f := FileAccess.open("user://filepicker_log.txt", FileAccess.WRITE)
	f.store_line("=== filepicker probe start ===")
	f.close()
	get_tree().create_timer(20.0).timeout.connect(func():
		_stamp("TIMEOUT")
		print("TIMEOUT")
		get_tree().quit(1)
	)
	_run()


func _run() -> void:
	var tests_dir := ProjectSettings.globalize_path("res://tests")
	var project_dir := ProjectSettings.globalize_path("res://").replace("\\", "/").get_base_dir()
	_check("env.tests_dir_exists", DirAccess.dir_exists_absolute(tests_dir), tests_dir)

	var picker := DshFilePicker.new()
	picker.bucket = "probe"
	add_child(picker)
	_check("instance.bar_built", picker._bar != null and picker._addr != null)
	_check("instance.dialog_built", picker._dialog != null)
	_check("wiring.addr_submit_connected", picker._addr.text_submitted.is_connected(picker._on_addr_submitted))

	# --- 打开（目录模式） ---
	picker.dir_selected.connect(func(d: String): _got_dir = d)
	picker.open({ "mode": "dir", "title": "probe", "start_dir": tests_dir })
	await _frames(2)
	_check("open.visible", picker._dialog.visible)
	_check("open.mode", picker._dialog.file_mode == FileDialog.FILE_MODE_OPEN_DIR)
	_check("open.start_dir", DshFilePicker.normalize_dir(picker._dialog.current_dir) == DshFilePicker.normalize_dir(tests_dir),
		picker._dialog.current_dir)
	_check("open.bar_visible", picker._bar.visible)
	_check("open.addr_synced", DshFilePicker.normalize_dir(picker._addr.text) == DshFilePicker.normalize_dir(tests_dir),
		picker._addr.text)

	# --- 地址栏导航（模拟粘贴路径回车） ---
	picker._addr.text = project_dir
	picker._on_addr_submitted(project_dir)
	await _frames(1)
	_check("addr.navigate", DshFilePicker.normalize_dir(picker._dialog.current_dir) == DshFilePicker.normalize_dir(project_dir),
		picker._dialog.current_dir)

	# --- 无效路径反馈：目录保持不变，不崩溃 ---
	picker._on_addr_submitted("Z:/definitely/not/exists/__probe__")
	await _frames(1)
	_check("addr.invalid_kept", DshFilePicker.normalize_dir(picker._dialog.current_dir) == DshFilePicker.normalize_dir(project_dir))

	# --- 键盘：BackSpace 上级（直调处理器，覆盖守卫与导航逻辑） ---
	var up := DshFilePicker.normalize_dir(DshFilePicker.normalize_dir(project_dir).get_base_dir())
	var ev := InputEventKey.new()
	ev.keycode = KEY_BACKSPACE
	ev.pressed = true
	picker._unhandled_key_input(ev)
	await _frames(1)
	_check("key.backspace_up", DshFilePicker.normalize_dir(picker._dialog.current_dir) == up,
		"%s -> %s (want %s)" % [project_dir, picker._dialog.current_dir, up])

	# --- 驱动真实选择：OK 按钮触发 dir_selected 中继 ---
	picker.open({ "mode": "dir", "title": "probe2", "start_dir": tests_dir })
	await _frames(2)
	var ob := picker._dialog.get_ok_button()
	_check("drive.ok_button_exists", ob is BaseButton)
	if ob is BaseButton:
		(ob as BaseButton).pressed.emit()
	await _frames(2)
	_check("drive.dir_selected_relayed", DshFilePicker.normalize_dir(_got_dir) == DshFilePicker.normalize_dir(tests_dir), _got_dir)
	_check("drive.deactivated_bar", not picker._bar.visible)

	# --- 最近记录持久化 ---
	var recents := picker.load_recents("probe")
	_check("recents.recorded", recents.size() > 0 and DshFilePicker.normalize_dir(recents[0]) == DshFilePicker.normalize_dir(tests_dir),
		str(recents))
	picker.clear_recents("probe")
	_check("recents.cleared", picker.load_recents("probe").is_empty())

	# --- 复用同一实例切换文件模式 ---
	picker.open({ "mode": "files", "title": "probe3", "start_dir": "" })
	await _frames(2)
	_check("reuse.files_mode", picker._dialog.file_mode == FileDialog.FILE_MODE_OPEN_FILES)
	picker.close()
	await _frames(1)

	# --- 结论 ---
	_stamp("=== DONE fails=%d ===" % _fails)
	print("FILEPICKER_DONE fails=%d" % _fails)
	get_tree().quit(0 if _fails == 0 else 1)


func _frames(n: int) -> void:
	for i in n:
		await get_tree().process_frame
