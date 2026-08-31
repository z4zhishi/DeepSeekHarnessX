extends RefCounted
class_name DshUIReconciler

## Keyed reconciler for the DSHX declarative UI runtime.
##
## Owns the mutation schedule between a Dictionary document (plain data, JSON
## round-trippable) and the live Godot Control tree:
##   * identity  — children are matched by their stable "key", so an update
##     reuses live nodes instead of rebuilding the tree (React-style keyed
##     reconciliation, synchronous and frame-free: every mutation is applied
##     before update() returns, which keeps headless tests deterministic).
##   * patching  — only props whose value actually changed are applied; the
##     last applied prop set is snapshotted in node metadata for exact diffs.
##   * unmount   — removals and detach use immediate free(), never queue_free,
##     so node counts are inspectable synchronously.
##
## Node state lives in node metadata (the META_* keys), so the reconciler
## itself is a stateless RefCounted: any number of hosts can share one
## instance, and dropping the reconciler never leaks tree-owned nodes.
##
## Document contract (the DshUIDocument AST shape):
##     { "type": str, "key": str, "props": {..}, "children": [..] }
##   * "key": children are reused when the key survives an update; keys may be
##     provided per node or via props["key"]. Missing keys fall back to
##     positional auto keys, which have NO identity across reorders — always
##     supply stable keys for dynamic lists.
##   * "props": values persist across updates unless overwritten (a prop absent
##     from the new doc keeps its previously applied value; documents should
##     carry their complete desired prop set).
##   * "type" change under a surviving "key" is a remount: the old subtree is
##     freed and a new node built, because a Godot node's class is immutable.
##   * "on_click": a Callable is connected verbatim; a String id is dispatched
##     through the `action` signal, which keeps documents pure serializable
##     data.
##
## Note: engine/ui_document.gd owns document helpers. This file still carries
## its own _normalize()/validate() so the reconciler compiles independently
## of global class-cache freshness.

# Cross-file types come from preload constants, NOT global class_name lookups:
# the registry script doubles as the parameter type, so this file compiles
# even before the project's global script class cache is (re)generated.
const DshRegistry := preload("res://engine/component_registry.gd")

## Node metadata keys (prefixed to stay collision-free with app metadata).
const META_TYPE := "_dsh_type"
const META_KEY := "_dsh_key"
const META_PROPS := "_dsh_props"
const META_CHILDREN := "_dsh_children"
const META_ROOT := "_dsh_root"
const META_CLICK_CB := "_dsh_click_cb"
const META_MODE := "_dsh_mode"

## Canonical prop application order. Directional flags (expand) are applied
## before the ratios so a single document always converges to the same final
## Control state regardless of prop order in the JSON.
const APPLY_ORDER := [
	"min_width", "min_height", "expand",
	"width_ratio", "height_ratio",
	"gap", "padding", "visible",
	"text", "tooltip", "bg", "border", "radius",
	"on_click", "mode",
	"placeholder", "disabled", "items",
]

## Emitted when a button bound to a String action id is pressed. The bridge
## keeps documents serializable: payloads never need to embed Callables.
signal action(action_name: String)

var registry: DshRegistry


func _init(p_registry: DshRegistry = null) -> void:
	registry = p_registry if p_registry != null else DshRegistry.new()
	# Fill builtin + interactive types only where still missing, so app-level
	# overrides (and Hero's "icon" factory) survive if they registered first.
	registry.register_builtins()
	registry.register_interactive()


# --- Public API --------------------------------------------------------------


## Mounts [param doc] into [param host], replacing any previous mount.
## Returns the materialized root Control, or null when nothing could be built.
func mount(host: Control, doc: Dictionary) -> Control:
	unmount(host)
	var stats := _blank_stats()
	var root := _materialize(doc, stats)
	if root != null:
		host.add_child(root)
		host.set_meta(META_ROOT, root)
	return root


## Reconciles the mounted tree under [param host] with [param doc].
## Mounts on the first call, diffs afterwards. Returns a stats dictionary:
##   created         nodes freshly materialized (whole counted subtrees)
##   reused          existing nodes re-entered by key (and subtree patched)
##   patched         individual prop applications whose value changed
##   removed         nodes freed, subtrees included
##   skipped         malformed nodes / unknown types ignored
##   unhandled       props present but inapplicable to the node kind
func update(host: Control, doc: Dictionary) -> Dictionary:
	var stats := _blank_stats()
	var current: Control = null
	if host.has_meta(META_ROOT):
		current = host.get_meta(META_ROOT)
	if current != null and not is_instance_valid(current):
		current = null
	if current == null:
		var mounted := _materialize(doc, stats)
		if mounted != null:
			host.add_child(mounted)
			host.set_meta(META_ROOT, mounted)
			# Children already booked their own counts during materialization;
			# only the root itself is counted here.
			stats["created"] += 1
		return stats
	var cdoc := _normalize(doc) as Dictionary
	if str(cdoc["type"]) == "" or not registry.has(cdoc["type"]):
		# An unrepresentable replacement root means "unmount".
		_unmount_current(host, current, stats)
		return stats
	if str(current.get_meta(META_TYPE)) != str(cdoc["type"]) \
			or str(current.get_meta(META_KEY)) != str(cdoc["key"]):
		# Root identity change: remount in place, keeping the slot index.
		var replacement := _materialize(cdoc, stats)
		if replacement != null:
			var at := current.get_index()
			host.add_child(replacement)
			host.move_child(replacement, at)
			_unmount_current(host, current, stats)
			host.set_meta(META_ROOT, replacement)
			# Only the root counts here; its subtree booked itself on materialize.
			stats["created"] += 1
		# On a failed materialize the old root is kept; stats["skipped"] tells
		# the caller the update did not apply.
	else:
		# The root node object survives the update: that IS the reuse contract.
		stats["reused"] += 1
		_reconcile_node(current, cdoc, stats)
	return stats


## Removes and frees the mounted tree under [param host].
## Returns the number of freed Controls. Uses immediate free() — never call
## from inside a signal handler of a node that is being freed.
func unmount(host: Control) -> int:
	if not host.has_meta(META_ROOT):
		return 0
	var root: Control = host.get_meta(META_ROOT)
	host.remove_meta(META_ROOT)
	if root == null or not is_instance_valid(root):
		return 0
	host.remove_child(root)
	var count := _subtree_count(root)
	root.free()
	return count


## Alias of unmount() for expressive call sites ("detach the runtime tree").
func detach(host: Control) -> int:
	return unmount(host)


## Engine-materialized Controls currently alive under [param host].
func node_count(host: Control) -> int:
	if not host.has_meta(META_ROOT):
		return 0
	var root: Control = host.get_meta(META_ROOT)
	if root == null or not is_instance_valid(root):
		return 0
	return _subtree_count(root)


## First materialized node under [param root] (inclusive) carrying [param key].
func find_by_key(root: Control, key: String) -> Control:
	if not is_instance_valid(root):
		return null
	if str(root.get_meta(META_KEY, "")) == key:
		return root
	for child in root.get_children():
		if child is Control:
			var hit := find_by_key(child as Control, key)
			if hit != null:
				return hit
	return null


## Validates a raw document tree. Returns problems (empty = valid). Same rules
## as the DshUIDocument contract: non-empty "type", Dictionary children only.
static func validate(raw: Dictionary) -> PackedStringArray:
	var problems := PackedStringArray()
	if not raw.has("type") or str(raw.get("type", "")).strip_edges() == "":
		problems.append("node is missing a 'type'")
	for c in raw.get("children", []):
		if c is Dictionary:
			problems.append_array(validate(c))
		else:
			problems.append("child of '%s' is not a Dictionary" % str(raw.get("type", "")))
	return problems


# --- Document normalization --------------------------------------------------


## Normalizes a raw node into {type, key, props, children} so later code paths
## can trust field presence. Recurses into children; key falls back to
## props["key"] (the convention DshUIDocument.node() documented).
func _normalize(raw: Dictionary) -> Dictionary:
	var props: Dictionary = {}
	var raw_props: Variant = raw.get("props", {})
	if raw_props is Dictionary:
		props = raw_props
	var children: Array = []
	var raw_children: Variant = raw.get("children", [])
	if raw_children is Array:
		for c in raw_children:
			if c is Dictionary:
				children.append(_normalize(c as Dictionary))
	var key := String(raw.get("key", ""))
	if key.is_empty() and props.has("key"):
		key = str(props["key"])
	return {
		"type": String(raw.get("type", "")).strip_edges(),
		"key": key,
		"props": props,
		"children": children,
	}


# --- Materialization ---------------------------------------------------------


## Builds (but does not attach) the Control for [param raw].
## Creation-time prop application is booked against `created` (the caller);
## `patched` counts only later diffs, keeping update metrics meaningful.
func _materialize(raw: Dictionary, stats: Dictionary) -> Control:
	var cdoc := _normalize(raw)
	if str(cdoc["type"]) == "" or not registry.has(cdoc["type"]):
		stats["skipped"] += 1
		return null
	var node: Control = registry.create(cdoc["type"], cdoc["props"])
	if node == null:
		stats["skipped"] += 1
		return null
	node.set_meta(META_TYPE, cdoc["type"])
	node.set_meta(META_KEY, cdoc["key"])
	var props: Dictionary = cdoc["props"]
	# Apply in canonical order so flag interactions (expand vs ratios) are
	# deterministic for documents that carry overlapping sizing props.
	for prop_name in APPLY_ORDER:
		if props.has(prop_name):
			_apply_prop(node, prop_name, props[prop_name])
	node.set_meta(META_PROPS, props.duplicate())
	# Recurse: children materialize and attach as engine-owned Controls of
	# this node (their counts book into the same stats).
	_sync_children(node, cdoc["children"], stats)
	return node


# --- Reconciliation ----------------------------------------------------------


## Diffs-and-applies props for an existing materialized [param node], then
## reconciles its children (recursion bottoming out in _sync_children).
## Only changed values are applied; props absent from the new doc persist.
func _reconcile_node(node: Control, cdoc: Dictionary, stats: Dictionary) -> void:
	var new_props: Dictionary = cdoc["props"]
	var old_props: Dictionary = {}
	if node.has_meta(META_PROPS):
		var stored: Variant = node.get_meta(META_PROPS)
		if stored is Dictionary:
			old_props = stored
	for prop_name in APPLY_ORDER:
		if not new_props.has(prop_name):
			continue
		if old_props.has(prop_name) and _props_equal(old_props[prop_name], new_props[prop_name]):
			continue
		if _apply_prop(node, prop_name, new_props[prop_name]):
			stats["patched"] += 1
		else:
			stats["unhandled"] += 1
	# Snapshot = union of previously applied and current props: props absent
	# from the new doc persist, so the snapshot must remember them too.
	var merged: Dictionary = old_props.duplicate()
	for prop_name in new_props:
		merged[prop_name] = new_props[prop_name]
	node.set_meta(META_PROPS, merged)
	_sync_children(node, cdoc["children"], stats)


## Keyed children diff. Surviving keys are reused (reconciled in place), new
## keys are materialized at their document position, stale keys are removed
## and freed immediately. Reordering uses move_child — nodes are never rebuilt.
func _sync_children(parent: Control, raw_children: Array, stats: Dictionary) -> void:
	var cdocs: Array = []
	for c in raw_children:
		if c is Dictionary:
			cdocs.append(_normalize(c as Dictionary))
		else:
			stats["skipped"] += 1
	var old_map: Dictionary = {}
	if parent.has_meta(META_CHILDREN):
		var stored: Variant = parent.get_meta(META_CHILDREN)
		if stored is Dictionary:
			old_map = stored.duplicate()
	var new_map: Dictionary = {}
	for i in cdocs.size():
		var cdoc: Dictionary = cdocs[i]
		var key := _stable_key(cdoc, i, new_map)
		var child_node: Control = null
		if old_map.has(key):
			child_node = old_map[key] as Control
			old_map.erase(key)
		# Same-key type swap is a remount, not a patch: a Godot node's class
		# cannot change, so identity cannot survive it either.
		if child_node != null and str(child_node.get_meta(META_TYPE, "")) != cdoc["type"]:
			_remove_child_node(parent, child_node, stats)
			child_node = null
		if child_node == null:
			child_node = _materialize(cdoc, stats)
			if child_node == null:
				continue # unknown type: node skipped, update carries on
			parent.add_child(child_node)
			stats["created"] += 1
		else:
			_reconcile_node(child_node, cdoc, stats)
			stats["reused"] += 1
		# Order enforcement: each surviving node is parked exactly at slot i;
		# stale children still in place shift right and are freed afterwards.
		if child_node.get_index() != i:
			parent.move_child(child_node, i)
		new_map[key] = child_node
	# Leftovers in old_map are stale: detach + immediate free.
	for stale_key in old_map:
		var stale: Control = old_map[stale_key] as Control
		if stale != null and is_instance_valid(stale):
			_remove_child_node(parent, stale, stats)
	parent.set_meta(META_CHILDREN, new_map)


## Builds the per-parent identity key for a child at [param index].
## Explicit keys win; duplicates inside one parent get disambiguated ("k#2")
## so a document bug can never silently corrupt the child map.
func _stable_key(cdoc: Dictionary, index: int, taken: Dictionary) -> String:
	var key := str(cdoc["key"])
	if key.is_empty():
		key = "auto/%s@%d" % [cdoc["type"], index]
	while taken.has(key):
		key = "%s#%d" % [key, index]
	return key


## Maps one prop onto node properties. Returns false for props that do not
## apply to this node kind — the Reconciler counts those as `unhandled`
## instead of aborting the whole update.
func _apply_prop(node: Control, prop: String, value: Variant) -> bool:
	match prop:
		"min_width":
			var cms := node.custom_minimum_size
			cms.x = to_f(value)
			node.custom_minimum_size = cms
			return true
		"min_height":
			var cms := node.custom_minimum_size
			cms.y = to_f(value)
			node.custom_minimum_size = cms
			return true
		"expand":
			if to_bool(value):
				node.size_flags_horizontal = Control.SIZE_EXPAND_FILL
				node.size_flags_vertical = Control.SIZE_EXPAND_FILL
			else:
				node.size_flags_horizontal = Control.SIZE_FILL
				node.size_flags_vertical = Control.SIZE_FILL
			return true
		"width_ratio":
			if to_f(value) > 0.0:
				node.size_flags_horizontal = Control.SIZE_EXPAND_FILL
				node.size_flags_stretch_ratio = to_f(value)
			else:
				node.size_flags_horizontal = Control.SIZE_FILL
				node.size_flags_stretch_ratio = 1.0
			return true
		"height_ratio":
			if to_f(value) > 0.0:
				node.size_flags_vertical = Control.SIZE_EXPAND_FILL
				node.size_flags_stretch_ratio = to_f(value)
			else:
				node.size_flags_vertical = Control.SIZE_FILL
				node.size_flags_stretch_ratio = 1.0
			return true
		"gap":
			if node is Container:
				node.add_theme_constant_override("separation", to_i(value))
				return true
			return false
		"padding":
			if node is PanelContainer:
				var m := to_f(value)
				var sb := panel_stylebox(node as PanelContainer)
				sb.content_margin_left = m
				sb.content_margin_top = m
				sb.content_margin_right = m
				sb.content_margin_bottom = m
				return true
			return false
		"visible":
			node.visible = to_bool(value)
			return true
		"text":
			if node is Label or node is BaseButton:
				node.set("text", str(value))
				return true
			if node is TextEdit:
				var te := node as TextEdit
				var next_text := str(value)
				# Assigning TextEdit.text resets the caret; skip a no-op write.
				if te.text != next_text:
					te.text = next_text
				return true
			if node is LineEdit:
				var le := node as LineEdit
				var next_line := str(value)
				if le.text != next_line:
					le.text = next_line
				return true
			return false
		"tooltip":
			node.tooltip_text = str(value)
			return true
		"bg":
			if node is PanelContainer:
				panel_stylebox(node as PanelContainer).bg_color = to_color(value)
				return true
			return false
		"border":
			if node is PanelContainer:
				var sb := panel_stylebox(node as PanelContainer)
				var color := to_color(value)
				sb.border_color = color
				# A fully transparent (or empty) border turns the frame off;
				# any real color draws a hairline border.
				var w := 0 if color.is_equal_approx(Color.TRANSPARENT) else 1
				sb.border_width_left = w
				sb.border_width_top = w
				sb.border_width_right = w
				sb.border_width_bottom = w
				return true
			return false
		"radius":
			if node is PanelContainer:
				panel_stylebox(node as PanelContainer).set_corner_radius_all(
						clampi(to_i(value), 0, 64))
				return true
			return false
		"on_click":
			return rewire_click(node, value)
		"mode":
			# Free-form per-node mode hint for app layers; the engine stores it
			# as metadata so documents remain plain data.
			node.set_meta(META_MODE, value)
			return true
		"placeholder":
			if node is TextEdit:
				(node as TextEdit).placeholder_text = str(value)
				return true
			if node is LineEdit:
				(node as LineEdit).placeholder_text = str(value)
				return true
			return false
		"disabled":
			if node is BaseButton:
				(node as BaseButton).disabled = to_bool(value)
				return true
			if node is TextEdit:
				(node as TextEdit).editable = not to_bool(value)
				return true
			if node is LineEdit:
				(node as LineEdit).editable = not to_bool(value)
				return true
			return false
		"items":
			if not (value is Array):
				return false
			if node is OptionButton:
				var opt := node as OptionButton
				opt.clear()
				for opt_entry in value:
					opt.add_item(str(opt_entry))
				return true
			if node is ItemList:
				var lst := node as ItemList
				lst.clear()
				for list_entry in value:
					lst.add_item(str(list_entry))
				return true
			return false
	return false


## Rebinds a click handler. The previous binding is disconnected first; a
## Callable value is connected verbatim, a String id routes through `action`.
## Called only when the diff decided the value actually changed.
func rewire_click(node: Control, value: Variant) -> bool:
	if not (node is BaseButton):
		return false
	var button := node as BaseButton
	var previous: Callable = node.get_meta(META_CLICK_CB, Callable())
	if previous.is_valid():
		button.pressed.disconnect(previous)
	var next := Callable()
	if value is Callable:
		next = value
	elif value is String and not (value as String).strip_edges().is_empty():
		next = Callable(self, "_emit_action").bind(String(value))
	if next.is_valid():
		button.pressed.connect(next)
		button.set_meta(META_CLICK_CB, next)
	else:
		button.remove_meta(META_CLICK_CB)
	return true


## Signal target for String action ids; re-emits on the reconciler so call
## sites subscribe once instead of tracking individual buttons.
func _emit_action(action_name: String) -> void:
	action.emit(action_name)


## Removes one engine-owned child from [param parent]: books its whole subtree
## as removed, detaches, and frees immediately (deterministic for tests).
func _remove_child_node(parent: Control, child: Control, stats: Dictionary) -> void:
	if child == null or not is_instance_valid(child):
		return
	stats["removed"] += _subtree_count(child)
	parent.remove_child(child)
	child.free()


## Frees the engine-rooted tree under [param host] and books it as removed.
func _unmount_current(host: Control, current: Control, stats: Dictionary) -> void:
	if current == null or not is_instance_valid(current):
		return
	stats["removed"] += _subtree_count(current)
	host.remove_child(current)
	current.free()


# --- Small utilities ---------------------------------------------------------


## Number of engine-owned Controls in the subtree rooted at [param node].
## Reconciler-created parents only ever contain engine children, so counting
## every Control is exact.
func _subtree_count(node: Control) -> int:
	var total := 1
	for c in node.get_children():
		if c is Control:
			total += _subtree_count(c as Control)
	return total


## Prop equality with type strictness: JSON payloads decode numbers as float,
## hand-built docs may carry int — 8 vs 8.0 re-patches once (harmless and
## idempotent) instead of inventing numeric tolerance in the diff.
func _props_equal(a: Variant, b: Variant) -> bool:
	return typeof(a) == typeof(b) and a == b


## Coerces JSON-friendly color values ("#rrggbb", "#rrggbbaa") into Color.
## Empty strings collapse to TRANSPARENT (used to switch a border off).
static func to_color(value: Variant) -> Color:
	if value is Color:
		return value as Color
	if value is String:
		var s := String(value).strip_edges()
		if s.is_empty():
			return Color.TRANSPARENT
		if s.is_valid_html_color():
			return Color.html(s)
	push_warning("DshUIReconciler: not a color value: %s" % str(value))
	return Color.TRANSPARENT


## Numeric coercion shared by sizing props (JSON numbers arrive as float).
static func to_f(value: Variant) -> float:
	if value is float or value is int:
		return float(value)
	return 0.0


static func to_i(value: Variant) -> int:
	return int(to_f(value))


static func to_bool(value: Variant) -> bool:
	return bool(value)


## The stylebox that bg/border/radius/padding mutate. Only PanelContainer backs
## a "panel" theme item, so other node kinds are rejected early — preventing
## accidental style overrides on Buttons/Labels.
func panel_stylebox(node: PanelContainer) -> StyleBoxFlat:
	if node == null:
		return null
	var sb := node.get_theme_stylebox("panel") as StyleBoxFlat
	if sb == null:
		sb = StyleBoxFlat.new()
		sb.anti_aliasing = false
		node.add_theme_stylebox_override("panel", sb)
	return sb


func _blank_stats() -> Dictionary:
	return {
		"created": 0, "reused": 0, "patched": 0,
		"removed": 0, "skipped": 0, "unhandled": 0,
	}