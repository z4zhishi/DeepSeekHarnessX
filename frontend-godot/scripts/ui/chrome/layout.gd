extends RefCounted
class_name DshChromeLayout

## Slot layout for composer/header chrome. Data only: widget ids per slot.
## user://chrome_layout.json overlays the defaults; unknown ids are dropped.

const PATH := "user://chrome_layout.json"

const DEFAULT := {
	"composer.left": ["approval", "reject_all"],
	"composer.right": ["model_effort", "attach", "send"],
	"composer.overflow": ["commands"],
}

const KNOWN := ["approval", "reject_all", "model_effort", "attach", "send", "commands"]

var _slots: Dictionary = {}
var _catalog = null


func _init(catalog = null) -> void:
	_catalog = catalog
	_slots = DEFAULT.duplicate(true)


func known_ids() -> PackedStringArray:
	if _catalog != null and _catalog.has_method("ids"):
		var got: Variant = _catalog.ids()
		if got is PackedStringArray and (got as PackedStringArray).size() > 0:
			return got as PackedStringArray
		if got is Array and (got as Array).size() > 0:
			var from_cat := PackedStringArray()
			for id in got:
				var s := str(id).strip_edges()
				if s != "":
					from_cat.append(s)
			if from_cat.size() > 0:
				return from_cat
	var out := PackedStringArray()
	for id in KNOWN:
		out.append(id)
	return out


func widgets_for(slot: String) -> PackedStringArray:
	var out := PackedStringArray()
	var known := known_ids()
	var raw: Variant = _slots.get(slot, [])
	if not (raw is Array):
		return out
	for item in raw:
		var id := str(item).strip_edges()
		if id != "" and known.has(id):
			out.append(id)
	return out


func load_layout() -> void:
	_slots = DEFAULT.duplicate(true)
	if not FileAccess.file_exists(PATH):
		return
	var f := FileAccess.open(PATH, FileAccess.READ)
	if f == null:
		return
	var parsed: Variant = JSON.parse_string(f.get_as_text())
	if not (parsed is Dictionary):
		return
	_merge(parsed as Dictionary)


func save_layout(data: Dictionary = {}) -> void:
	if not data.is_empty():
		_merge(data)
	var f := FileAccess.open(PATH, FileAccess.WRITE)
	if f == null:
		return
	f.store_string(JSON.stringify(_slots))


func as_dict() -> Dictionary:
	return _slots.duplicate(true)


func _merge(over: Dictionary) -> void:
	for key in over.keys():
		var slot := str(key)
		if not DEFAULT.has(slot):
			continue
		var ids: Array = []
		var raw: Variant = over[key]
		if raw is Array:
			var known := known_ids()
			for item in raw:
				var id := str(item).strip_edges()
				if id != "" and known.has(id) and not ids.has(id):
					ids.append(id)
		if not ids.is_empty():
			_slots[slot] = ids


func reload_from_disk() -> void:
	load_layout()
