extends SceneTree

## SceneTree wrapper: --script mode requires SceneTree. Real asserts live in
## probe_chrome_editor_node.gd (wrapper + node, same convention as hero).

func _init() -> void:
	call_deferred("_boot")


func _boot() -> void:
	var probe_script: GDScript = load("res://tests/probe_chrome_editor_node.gd")
	if probe_script == null:
		print("CHROME_EDITOR_RESULT passed=0 failed=1 reason=load")
		quit(1)
		return
	var node: Node = probe_script.new()
	root.add_child(node)
