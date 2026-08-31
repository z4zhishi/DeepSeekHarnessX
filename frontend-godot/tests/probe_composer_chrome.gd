extends SceneTree

## Instantiates the live composer scene and checks IA chrome:
## approval at left, model+effort button, reject-all, overflow.

func _init() -> void:
	call_deferred("_boot")


func _boot() -> void:
	var node := Node.new()
	root.add_child(node)
	var script: GDScript = load("res://tests/probe_composer_chrome_node.gd")
	var runner: Node = script.new()
	node.add_child(runner)
