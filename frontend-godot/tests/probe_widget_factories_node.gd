extends Node

## Headless probe for DshUIWidgets interactive factories + additive reconciler props.
##
## Drives the REAL runtime path — a Control host, DshUIReconciler.new()
## (register_builtins + register_interactive in _init), then registry.create
## AND recon.mount of a tiny document using the new types.
##
## Verdict line (grep this in CI):
##     WIDGET_FACTORIES_RESULT passed=<P> failed=<F>

const DshReconcilerT := preload("res://engine/reconciler.gd")

var _passed := 0
var _failed := 0


func _assert(cond: bool, msg: String) -> void:
	if cond:
		_passed += 1
		print("  PASS: ", msg)
	else:
		_failed += 1
		print("  FAIL: ", msg)


func _ready() -> void:
	await _run()
	print("WIDGET_FACTORIES_RESULT passed=%d failed=%d" % [_passed, _failed])
	get_tree().quit(1 if _failed > 0 else 0)


func _run() -> void:
	await get_tree().process_frame
	var host := Control.new()
	host.name = "WidgetHost"
	add_child(host)
	var recon: DshReconcilerT = DshReconcilerT.new()
	var registry = recon.registry

	print("== registry.has ==")
	_assert(registry.has("text_input"), "text_input is registered")
	_assert(registry.has("chip"), "chip is registered")
	_assert(registry.has("icon_button"), "icon_button is registered")
	_assert(registry.has("dropdown"), "dropdown is registered")
	_assert(registry.has("list"), "list is registered")
	_assert(not registry.has("composer"), "composer is not registered")
	_assert(not registry.has("file_picker"), "file_picker is not registered")
	_assert(not registry.has("menu"), "menu is not registered")

	print("== registry.create ==")
	var created_input: Control = registry.create("text_input", {})
	_assert(created_input is TextEdit, "create(text_input) is TextEdit")
	var created_chip: Control = registry.create("chip", {})
	_assert(created_chip is Button, "create(chip) is Button")
	var created_dropdown: Control = registry.create("dropdown", {})
	_assert(created_dropdown is OptionButton, "create(dropdown) is OptionButton")
	var created_icon_button: Control = registry.create("icon_button", {})
	_assert(created_icon_button is Button, "create(icon_button) is Button")
	var created_list: Control = registry.create("list", {})
	_assert(created_list is ItemList, "create(list) is ItemList")
	_assert(registry.create("definitely-not-a-type", {}) == null, "unknown type create() returns null")
	for scratch in [created_input, created_chip, created_dropdown, created_icon_button, created_list]:
		if scratch != null:
			scratch.free()

	print("== mount ==")
	var doc := {
		"type": "column",
		"key": "root",
		"props": {},
		"children": [
			{"type": "text_input", "key": "in", "props": {"text": "hello", "placeholder": "type here"}},
			{"type": "chip", "key": "chip", "props": {"text": "Access"}},
			{"type": "dropdown", "key": "dd", "props": {"items": ["alpha", "beta"]}},
			{"type": "mystery-widget", "key": "bad", "props": {}},
		],
	}
	var root: Control = recon.mount(host, doc)
	_assert(root is VBoxContainer, "mount root is the column (VBoxContainer)")
	_assert(root != null and root.get_child_count() == 3, "unknown type skipped; 3 real children mounted")
	_assert(root != null and root.get_child(0) is TextEdit, "mounted text_input is TextEdit")
	_assert(root != null and root.get_child(1) is Button, "mounted chip is Button")
	_assert(root != null and root.get_child(2) is OptionButton, "mounted dropdown is OptionButton")
	_assert(recon.find_by_key(host, "bad") == null, "unknown type is not in the tree")
	var mounted_input := recon.find_by_key(host, "in") as TextEdit
	_assert(mounted_input != null and mounted_input.text == "hello", "text prop applied to TextEdit")
	_assert(mounted_input != null and mounted_input.placeholder_text == "type here", "placeholder prop applied to TextEdit")
	var mounted_dd := recon.find_by_key(host, "dd") as OptionButton
	_assert(mounted_dd != null and mounted_dd.item_count == 2 and mounted_dd.get_item_text(0) == "alpha",
			"items prop applied to OptionButton")

	print("== patch reuse ==")
	var chip_node := recon.find_by_key(host, "chip") as Button
	_assert(chip_node != null and chip_node.text == "Access", "chip mounted with text")
	var chip_id := -1
	if chip_node != null:
		chip_id = chip_node.get_instance_id()
	var doc2: Dictionary = doc.duplicate(true)
	(doc2["children"][1]["props"] as Dictionary)["text"] = "Patched"
	var stats: Dictionary = recon.update(host, doc2)
	var chip_after := recon.find_by_key(host, "chip") as Button
	_assert(chip_after != null and chip_after.get_instance_id() == chip_id,
			"update() patching text on chip reuses the same instance id")
	_assert(chip_after != null and chip_after.text == "Patched", "patched chip text applied")
	_assert(int(stats["created"]) == 0, "text patch creates nothing")
	_assert(int(stats["skipped"]) == 1, "unknown type still skipped on update")

	recon.unmount(host)
	host.free()
