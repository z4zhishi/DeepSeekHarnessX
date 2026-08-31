extends SceneTree

## SceneTree 包装器：--script 模式要求 SceneTree 继承；实际探针逻辑在
## probe_widget_factories_node.gd（wrapper + node 两文件约定）。

func _init() -> void:
	call_deferred("_boot")


func _boot() -> void:
	var probe_script: GDScript = load("res://tests/probe_widget_factories_node.gd")
	if probe_script == null:
		print("WIDGET_FACTORIES_RESULT passed=0 failed=1")
		quit(1)
		return
	var node: Node = probe_script.new()
	root.add_child(node)
