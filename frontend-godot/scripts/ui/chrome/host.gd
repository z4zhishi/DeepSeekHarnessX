extends RefCounted
class_name DshChromeHost

## Mounts named chrome widgets into slot containers using a DshChromeLayout.
## Factories return an existing Control (composer registers its built buttons).

var _factories: Dictionary = {}


func register(id: String, factory: Callable) -> void:
	var key := id.strip_edges()
	if key == "" or factory == null or not factory.is_valid():
		return
	_factories[key] = factory


func resolve(id: String) -> Control:
	var factory: Callable = _factories.get(id, Callable())
	if not factory.is_valid():
		return null
	var out: Variant = factory.call()
	return out as Control


func mount(slot_node: Control, slot_id: String, layout) -> Array[String]:
	var mounted: Array[String] = []
	if slot_node == null or layout == null:
		return mounted
	var i := 0
	for id in layout.widgets_for(slot_id):
		var node := resolve(id)
		if node == null:
			continue
		node.visible = true
		var parent := node.get_parent()
		if parent != slot_node:
			if parent != null:
				parent.remove_child(node)
			slot_node.add_child(node)
		slot_node.move_child(node, mini(i, slot_node.get_child_count() - 1))
		mounted.append(id)
		i += 1
	return mounted
