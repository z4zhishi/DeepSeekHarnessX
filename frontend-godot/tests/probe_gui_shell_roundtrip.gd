extends SceneTree

## 场景/事件双路径（mux WS 帧形态）走真实 app.gd 处理链 —— wrapper 同款。
## 实际逻辑在 probe_gui_shell_roundtrip_node.gd。

func _init() -> void:
	call_deferred("_boot")


func _boot() -> void:
	var holder := Node.new()
	root.add_child(holder)
	var script: GDScript = load("res://tests/probe_gui_shell_roundtrip_node.gd")
	if script == null or not script.can_instantiate():
		print("GUI_SHELL_RESULT passed=0 failed=1 reason=script-load-failed")
		quit(1)
		return
	var runner: Node = script.new()
	holder.add_child(runner)