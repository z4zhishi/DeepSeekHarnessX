extends SceneTree

## SceneTree 包装器：委托实际的 GUI structure 探针节点执行。
## --script 模式要求 SceneTree 继承；实际逻辑在 probe_gui_structure_node.gd。

func _init() -> void:
	var node: Node = load("res://tests/probe_gui_structure_node.gd").new()
	root.add_child(node)