extends RefCounted
class_name DshChromeCatalog

## Builtin + plugin chrome widget ids. Layout slots store these ids;
## the host resolves them to Controls via factories.

const BUILTIN := [
	{"id": "approval", "title_key": "chat.approval", "fallback": "审批等级", "default_slot": "composer.left"},
	{"id": "reject_all", "title_key": "chat.rejectAll", "fallback": "自动拒绝审批", "default_slot": "composer.left"},
	{"id": "model_effort", "title_key": "chat.modelEffort", "fallback": "模型与思考等级", "default_slot": "composer.right"},
	{"id": "attach", "title_key": "chat.attach", "fallback": "添加附件", "default_slot": "composer.right"},
	{"id": "send", "title_key": "chat.send", "fallback": "发送", "default_slot": "composer.right"},
	{"id": "commands", "title_key": "chat.commands", "fallback": "命令", "default_slot": "composer.overflow"},
]

var _items: Dictionary = {}


func _init() -> void:
	for raw in BUILTIN:
		if raw is Dictionary:
			register(str((raw as Dictionary).get("id", "")), raw as Dictionary)


func register(id: String, desc: Dictionary) -> void:
	var key := id.strip_edges()
	if key == "":
		return
	var item := desc.duplicate(true)
	item["id"] = key
	if not item.has("title_key"):
		item["title_key"] = ""
	if not item.has("fallback"):
		item["fallback"] = key
	if not item.has("default_slot"):
		item["default_slot"] = "composer.overflow"
	_items[key] = item


func ids() -> PackedStringArray:
	var out := PackedStringArray()
	for key in _items.keys():
		out.append(str(key))
	return out


func descriptor(id: String) -> Dictionary:
	var raw: Variant = _items.get(id, {})
	if raw is Dictionary:
		return (raw as Dictionary).duplicate(true)
	return {}
