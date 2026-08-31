extends SceneTree

## SceneTree 包装器：委托实际的 GUI 壳探针节点执行。
## --script 模式要求 SceneTree 继承；实际逻辑在 probe_gui_shell_node.gd。
## RESULT：GUI_SHELL_RESULT passed=N failed=M

func _init() -> void:
	call_deferred("_boot")


func _boot() -> void:
	var holder := Node.new()
	root.add_child(holder)
	var script: GDScript = load("res://tests/probe_gui_shell_node.gd")
	if script == null or not script.can_instantiate():
		print("GUI_SHELL_RESULT passed=0 failed=1 reason=script-load-failed")
		quit(1)
		return
	var runner: Node = script.new()
	holder.add_child(runner)