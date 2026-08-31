extends SceneTree

## 习惯链探针 wrapper：实际逻辑在 probe_gui_shell_habits_node.gd。
## 脚本装载失败时直接给红 RESULT 退出，不把主循环挂死。

func _init() -> void:
	call_deferred("_boot")


func _boot() -> void:
	var holder := Node.new()
	root.add_child(holder)
	var script: GDScript = load("res://tests/probe_gui_shell_habits_node.gd")
	if script == null or not script.can_instantiate():
		print("GUI_SHELL_RESULT passed=0 failed=1 reason=script-load-failed")
		quit(1)
		return
	var runner: Node = script.new()
	holder.add_child(runner)