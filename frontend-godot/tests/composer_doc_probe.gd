extends SceneTree

## SceneTree 包装器：委托实际的 composer 文档探针节点执行。
## --script 模式要求 SceneTree 继承；实际断言逻辑在 composer_doc_probe_node.gd。
## 运行：godot --headless --path frontend-godot --script res://tests/composer_doc_probe.gd

func _init() -> void:
	var node: Node = load("res://tests/composer_doc_probe_node.gd").new()
	root.add_child(node)