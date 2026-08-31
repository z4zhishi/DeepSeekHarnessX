extends SceneTree

## Instantiates the live composer scene and proves the W10-c engine-mount
## runtime contract by script (no screenshots):
##   1. %X members are reconciler-factory products (engine mount, not scene nodes)
##   2. refreshes (apply_tokens x2 / reload_chrome) reuse node instances
##   3. chrome IA + behavior API roundtrips hold on the mounted tree
##   6. script-triggered behaviors reach the declared signals
##
## Verdict line (grep this in CI):
##     PROBE_COMPOSER_RUNTIME_RESULT passed=<p> failed=<f>

func _init() -> void:
	call_deferred("_boot")


func _boot() -> void:
	var node := Node.new()
	root.add_child(node)
	var script: GDScript = load("res://tests/probe_composer_runtime_node.gd")
	var runner: Node = script.new()
	node.add_child(runner)