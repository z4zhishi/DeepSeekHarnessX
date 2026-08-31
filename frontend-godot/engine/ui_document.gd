extends RefCounted
class_name DshUIDocument

## Declarative UI document AST.
##
## The whole engine is driven by Dictionary trees of this shape:
##   {
##     "type": "column",
##     "key": "sidebar",           # stable identity for node reuse
##     "props": {"min_width": 280.0},
##     "children": [ ...same shape... ]
##   }
##
## A document is plain data (JSON round-trippable). Nothing in this file knows
## about Godot node classes: the ComponentRegistry owns that mapping and the
## Reconciler owns the mutation schedule. Documents are plain Dictionaries by
## design — no custom classes, no scene files, no script loading at runtime.

const PROP_TYPES := [
	"min_width", "min_height", "expand",
	"width_ratio", "height_ratio",
	"gap", "padding", "visible",
	"text", "tooltip", "bg", "border", "radius",
	"on_click", "mode",
]


static func node(type: String, props: Dictionary = {}, children: Array = [], key: String = "") -> Dictionary:
	var resolved := str(key).strip_edges()
	if resolved == "":
		resolved = str(props.get("key", "")).strip_edges()
	if resolved == "":
		resolved = key_prop_for(type)
	return {
		"type": str(type),
		"key": resolved,
		"props": props,
		"children": children,
	}


static func key_prop_for(type_name: String) -> String:
	return "key_" + type_name


static func key_prop_for_named(type: String, name_hint: String) -> String:
	return key_prop_for(type) + "_" + name_hint


## Validates a document node: known type field, no non-Dictionary children,
## string key. Returns PackedStringArray of problems (empty = valid).
static func validate(n: Dictionary) -> PackedStringArray:
	var errs := PackedStringArray()
	if not n.has("type") or str(n.get("type", "")) == "":
		errs.append("node is missing a 'type'")
	for c in n.get("children", []):
		if c is Dictionary:
			errs.append_array(validate(c as Dictionary))
	return errs


static func text(value: String) -> Dictionary:
	return node("text", {"text": value})


static func button(label: String, on_click: String) -> Dictionary:
	return node("button", {"text": label, "on_click": on_click})