extends Node

## tests/probe_gui_shell_base.gd — GUI 壳/习惯链探针共用基座（仅测试文件）。
##
## 结构沿用 tests/probe_gui_structure_node.gd 的成熟做法：app.tscn 整树实例化，
## 唯一产品 seam 是 tests/mock_scripted_client.gd（给 %DshClient 换脚本），
## 其余 app.gd / composer.gd / store.gd / overlays 全部走真实路径。
## W0 约束：探针为 SceneTree wrapper + probe 节点；计数器只经成员方法改写；
## 无 RESULT 行即失败。

const REJECT_PATH := "user://approval_auto_reject.txt"
const PINS_PATH := "user://pinned_sessions.json"

var _passed := 0
var _failed := 0
var _finished := false

var app: Control = null
var client: Node = null

# —— user:// 持久文件备份/恢复（探针不许污染真实用户状态） ————————
var _reject_had_file := false
var _reject_backup := ""
var _pins_had_file := false
var _pins_backup := ""


func _ready() -> void:
	# 失败保险：探针任何未捕获异常/死锁都会在 120s 强制退出并置红，
	# 不把主循环挂死（无 RESULT 行 = 失败，与 Goal §0 一致）。
	get_tree().create_timer(120.0).timeout.connect(_on_failsafe)
	await get_tree().process_frame
	await _run()
	_cleanup_files()
	# 先收掉整棵 app 树（布局 timer/行池/弹层），再打 RESULT 并退出。
	if app != null and is_instance_valid(app):
		app.queue_free()
		await _frames(3)
	_finished = true
	print("%s passed=%d failed=%d" % [result_tag(), _passed, _failed])
	get_tree().quit(1 if _failed > 0 else 0)


func _on_failsafe() -> void:
	if _finished:
		return
	_finished = true
	print("%s passed=%d failed=%d reason=failsafe-timeout" % [result_tag(), _passed, _failed + 1])
	get_tree().quit(1)


## 子探针各自的 RESULT 标签。
func result_tag() -> String:
	return "GUI_SHELL_RESULT"


func _run() -> void:
	pass


func _assert(cond: bool, msg: String) -> void:
	if cond:
		_passed += 1
		print("  [PASS] %s" % msg)
	else:
		_failed += 1
		print("  [FAIL] %s" % msg)


func _frames(n: int) -> void:
	for _i in n:
		await get_tree().process_frame


## 实例化真实 app.tscn + mock 网关；sessions/histories/models 为脚本数据。
## mock 的 RPC 全部同步回调，返回时 app 已完成建会话 + 历史装载。
func boot_app(sessions: Array, histories: Dictionary, models: Array) -> bool:
	var packed: PackedScene = load("res://scenes/app.tscn")
	if packed == null:
		_assert(false, "app.tscn loads")
		return false
	app = packed.instantiate()
	var node := app.get_node_or_null("DshClient")
	if node == null:
		_assert(false, "%DshClient exists in app.tscn")
		app.free()
		return false
	node.set_script(load("res://tests/mock_scripted_client.gd"))
	client = node
	var session_copy: Array = sessions.duplicate(true)
	var history_copy: Dictionary = histories.duplicate(true)
	var models_copy: Array = models.duplicate(true)
	var pick := _selected_model_id(models)
	client.call("script_response", "session.list", func(_p: Dictionary, cb: Callable) -> void:
		cb.call(true, {"sessions": session_copy}))
	client.call("script_response", "session.history", func(p: Dictionary, cb: Callable) -> void:
		var sid := str(p.get("sessionId", ""))
		var events: Array = session_history(histories, sid)
		cb.call(true, {"events": events}))
	client.call("script_response", "provider.models", func(_p: Dictionary, cb: Callable) -> void:
		cb.call(true, {"models": models_copy, "selected": pick}))
	add_child(app)
	await _frames(12)
	return true


## 静态方法形态的事件查表（lambda 内直接闭包会碰 GDScript 捕获边界，
## 用成员函数最稳）。
func session_history(histories: Variant, sid: String) -> Array:
	if histories is Dictionary:
		var events: Variant = (histories as Dictionary).get(sid, [])
		if events is Array:
			return (events as Array).duplicate(true)
	return []


func _selected_model_id(_models: Array) -> String:
	return ""


## mock 侧 RPC 记录的包装（client 字段静态类型是 Node，探针一律走 helper）。
func client_rpc_calls(method: String) -> Array:
	var calls: Variant = client.call("rpc_calls", method)
	return calls if calls is Array else []


func client_rpc_clear() -> void:
	var entries: Variant = client.get("rpc_log")
	if entries is Array:
		(entries as Array).clear()


## app.gd 折叠视图（ConversationFold 产物）只读视图。
func chat_nodes() -> Array:
	if app == null:
		return []
	var chat: Node = app.get_node_or_null("%ChatList")
	if chat == null:
		return []
	var v: Variant = chat.get("_nodes")
	return v if v is Array else []


func chat_kinds() -> Array:
	var out: Array = []
	for n in chat_nodes():
		if n is Dictionary:
			out.append(str((n as Dictionary).get("kind", "")))
	return out


## 构造 app._unhandled_input 会处理的按下键事件（只断言处理器，不模拟键盘）。
func key_event(keycode_val: int, ctrl: bool, shift: bool = false) -> InputEventKey:
	var ev := InputEventKey.new()
	ev.keycode = (keycode_val as Key)
	ev.physical_keycode = (keycode_val as Key)
	ev.pressed = true
	ev.echo = false
	ev.ctrl_pressed = ctrl
	ev.meta_pressed = false
	ev.shift_pressed = shift
	return ev


# —— user:// 持久文件备份/恢复 ————————————————————————————————————

func snapshot_user_files() -> void:
	_reject_had_file = FileAccess.file_exists(REJECT_PATH)
	if _reject_had_file:
		_reject_backup = FileAccess.get_file_as_string(REJECT_PATH)
	_pins_had_file = FileAccess.file_exists(PINS_PATH)
	if _pins_had_file:
		_pins_backup = FileAccess.get_file_as_string(PINS_PATH)


func write_pins(ids: Array) -> void:
	var f := FileAccess.open(PINS_PATH, FileAccess.WRITE)
	if f != null:
		f.store_string(JSON.stringify(ids))
		f.close()


func _cleanup_files() -> void:
	_restore_file(REJECT_PATH, _reject_had_file, _reject_backup)
	_restore_file(PINS_PATH, _pins_had_file, _pins_backup)


func _restore_file(path: String, had: bool, backup: String) -> void:
	if had:
		var w := FileAccess.open(path, FileAccess.WRITE)
		if w != null:
			w.store_string(backup)
			w.close()
	elif FileAccess.file_exists(path):
		DirAccess.remove_absolute(path)