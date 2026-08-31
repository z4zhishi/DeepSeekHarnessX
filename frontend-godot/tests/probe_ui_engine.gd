extends Node

## Integration probe for the DSHX UI engine (frontend-godot/engine).
##
## Drives the full runtime pipeline headlessly:
##   1. registry  — builtin resolution + unknown-type safety
##   2. JSON load — a real .json document is parsed into the Dictionary AST
##   3. mount     — first render materializes the whole tree
##   4. patch     — an incremental update REUSES nodes (instance ids preserved),
##                  applies only the changed props, touches nothing else
##   5. reorder   — key-based reordering moves nodes, never rebuilds them
##   6. removal   — detached subtrees are freed immediately (deterministic)
##   7. insert    — new keys materialize exactly once
##   8. type swap — same key, new type: remount (identity cannot survive)
##   9. events    — String action ids dispatch through reconciler.action;
##                  Callables connect directly
##  10. unmount   — the whole engine tree frees; host child count returns to 0
##  11. layout    — WIDE/COMPACT/NARROW/TINY boundary rules (inclusive bounds)
##  12. viewport  — rules_for_viewport reacts to live viewport width changes
##
## Verdict line (grep this in CI):
##     UI_ENGINE_RESULT passed=<P> failed=<F>

const DshRegistryT := preload("res://engine/component_registry.gd")
const DshReconcilerT := preload("res://engine/reconciler.gd")
const DshLayoutT := preload("res://engine/layout_engine.gd")

const DOC_PATH := "res://tests/ui_engine_doc.json"

## Node total of the fixture document (see ui_engine_doc.json):
## root + panel + 2 texts + row + 2 buttons + status = 8.
const DOC_TOTAL_NODES := 8

var _recon: DshReconcilerT = null
var _eng: DshLayoutT = null
var _host: Control = null
var _doc: Dictionary = {}
var _passed := 0
var _failed := 0
var _action_log: Array = []
var _cb_hit := false


func _assert(cond: bool, msg: String) -> void:
	if cond:
		_passed += 1
		print("  PASS: ", msg)
	else:
		_failed += 1
		print("  FAIL: ", msg)


func _fresh_doc() -> Dictionary:
	return _doc.duplicate(true)


## Loads the fixture document from disk (real JSON, no embedded strings, so
## the test also proves the engine consumes JSON payloads end to end).
func _load_doc() -> Dictionary:
	var f := FileAccess.open(DOC_PATH, FileAccess.READ)
	if f == null:
		push_error("probe: cannot open %s" % DOC_PATH)
		return {}
	var parsed: Variant = JSON.parse_string(f.get_as_text())
	f.close()
	if parsed is Dictionary:
		return parsed as Dictionary
	push_error("probe: fixture JSON did not parse into a Dictionary")
	return {}


## Collects instance ids of every engine-owned Control under [param node].
func _collect_ids(node: Node, out: Array) -> void:
	for child in node.get_children():
		if child is Control:
			out.append((child as Control).get_instance_id())
			_collect_ids(child, out)


func _live_ids(node: Node) -> Array:
	var ids: Array = []
	_collect_ids(node, ids)
	return ids


func _same_set(a: Array, b: Array) -> bool:
	if a.size() != b.size():
		return false
	for v in a:
		if not b.has(v):
			return false
	return true


func _wait_frames(n: int) -> void:
	for _i in n:
		await get_tree().process_frame


## Connected to reconciler.action in Phase 9.
func _on_action(action_name: String) -> void:
	_action_log.append(action_name)


## Plain-Callable on_click target for Phase 9.
func _on_plain_callback() -> void:
	_cb_hit = true


func _ready() -> void:
	_run()


func _run() -> void:
	var registry: DshRegistryT = DshRegistryT.new()
	_recon = DshReconcilerT.new(registry)
	_eng = DshLayoutT.new()
	_host = Control.new()
	_host.name = "EngineHost"
	add_child(_host)
	_doc = _load_doc()

	# --- Phase 1: registry ---------------------------------------------------
	print("== registry ==")
	_assert(registry.has("column") and registry.has("button"), "builtin types registered")
	_assert(registry.resolve("definitely-not-a-type").is_valid() == false,
			"unknown type resolves to an invalid Callable")
	var probe_column := registry.create("column", {})
	_assert(probe_column is VBoxContainer, "column builds a VBoxContainer")
	# Factory products are not tree-owned: free the scratch node so the probe
	# exits without orphaned ObjectDB instances.
	probe_column.free()
	_assert(registry.create("definitely-not-a-type", {}) == null, "unknown type create() returns null")

	# --- Phase 2: JSON document load + first mount ---------------------------
	print("== json document mount ==")
	_assert(not _doc.is_empty(), "fixture JSON parsed into a Dictionary AST")
	_assert(DshReconcilerT.validate(_doc).is_empty(), "fixture document passes validate()")
	var stats: Dictionary = _recon.update(_host, _doc)
	_assert(int(stats["created"]) == DOC_TOTAL_NODES, "first mount creates all %d nodes (got %d)" % [DOC_TOTAL_NODES, int(stats["created"])])
	_assert(_recon.node_count(_host) == DOC_TOTAL_NODES, "node_count reports the mounted subtree")
	var root: Control = _host.get_child(0) as Control
	_assert(root is VBoxContainer, "root column materializes as VBoxContainer")
	_assert(root.get_child_count() == 3, "root has 3 children")
	_assert(int(root.get_theme_constant("separation")) == 12, "gap prop lands as separation theme constant")
	var hero: Control = _recon.find_by_key(_host, "hero-card")
	_assert(hero is PanelContainer, "hero-card materializes as PanelContainer")
	if hero is PanelContainer:
		var sb := (hero as PanelContainer).get_theme_stylebox("panel") as StyleBoxFlat
		_assert(sb != null and sb.bg_color == Color("#232324"), "bg hex prop applied to panel stylebox")
		_assert(sb != null and sb.border_color == Color("#4176e6") and sb.border_width_left == 1, "border hex prop applied")
		_assert(sb != null and sb.corner_radius_top_left == 12, "radius prop applied")
		_assert(hero.custom_minimum_size.y == 64.0, "min_height prop applied")
	var hero_sub := _recon.find_by_key(_host, "hero-sub") as Label
	_assert(hero_sub != null and hero_sub.text == "UI runtime probe", "text prop applied")
	_assert(hero_sub.tooltip_text == "hover me", "tooltip prop applied")
	var send_btn := _recon.find_by_key(_host, "btn-send") as Button
	_assert(send_btn != null and send_btn.text == "Send", "button text applied and keyed lookup works")

	# --- Phase 3: incremental patch reuses nodes ------------------------------
	print("== incremental patch (nodes must be reused) ==")
	var root_before := root
	var hero_before := hero
	var label_before := hero_sub
	var status_before := _recon.find_by_key(_host, "status") as Label
	var tree_before := _live_ids(_host)
	var doc2 := _fresh_doc()
	(doc2["props"] as Dictionary)["gap"] = 16
	(doc2["children"][2]["props"] as Dictionary)["text"] = "Status: online"
	stats = _recon.update(_host, doc2)
	_assert(int(stats["created"]) == 0, "patch creates nothing")
	_assert(int(stats["removed"]) == 0, "patch removes nothing")
	_assert(int(stats["reused"]) == DOC_TOTAL_NODES, "every node re-entered by key (reused=%d)" % int(stats["reused"]))
	_assert(int(stats["patched"]) == 2, "exactly the 2 changed props applied (patched=%d)" % int(stats["patched"]))
	_assert(root == root_before, "root node object is reused, not rebuilt")
	_assert(hero == hero_before, "hero panel node object is reused")
	_assert(hero_sub == label_before, "hero-sub label instance preserved (same instance id)")
	_assert(status_before.text == "Status: online", "changed text prop applied to the reused node")
	_assert(int(root.get_theme_constant("separation")) == 16, "changed gap prop applied")
	_assert(label_before.tooltip_text == "hover me", "untouched prop not re-applied but retained")
	var hero_sb := (hero as PanelContainer).get_theme_stylebox("panel") as StyleBoxFlat
	_assert(hero_sb != null and hero_sb.bg_color == Color("#232324"), "panel bg prop untouched by unrelated patch")
	_assert(_same_set(tree_before, _live_ids(_host)), "no node identity changed during a value-only patch")

	# --- Phase 4: reorder -----------------------------------------------------
	print("== reorder (keys, not rebuilds) ==")
	var doc3 := doc2.duplicate(true)
	var moved: Variant = (doc3["children"] as Array).pop_back()
	(doc3["children"] as Array).insert(0, moved)
	var before_reorder := _live_ids(_host)
	stats = _recon.update(_host, doc3)
	_assert(int(stats["created"]) == 0 and int(stats["removed"]) == 0, "reorder neither creates nor frees")
	_assert(int(stats["reused"]) == DOC_TOTAL_NODES, "reorder reconciles every node (reused=%d)" % int(stats["reused"]))
	_assert(_recon.find_by_key(_host, "status").get_index() == 0, "status moved to slot 0")
	_assert(root.get_child(1) == hero and root.get_child(2) == _recon.find_by_key(_host, "actions"), "panel and actions rows keep identity and follow document order")
	_assert(_same_set(tree_before, _live_ids(_host)), "reorder keeps every instance alive")

	# --- Phase 5: removal frees immediately -----------------------------------
	print("== removal ==")
	var doomed := _recon.find_by_key(_host, "hero-card") as Control
	var doc4 := doc3.duplicate(true)
	# After the reorder the document is [status, hero-card, actions]; drop the panel.
	(doc4["children"] as Array).pop_at(1)
	stats = _recon.update(_host, doc4)
	_assert(not is_instance_valid(doomed), "removed panel freed synchronously (not queue_free)")
	_assert(int(stats["removed"]) == 3, "removal books the whole subtree (removed=%d)" % int(stats["removed"]))
	_assert(int(stats["created"]) == 0, "removal creates nothing")
	_assert(_recon.node_count(_host) == DOC_TOTAL_NODES - 3, "tree shrinks by the removed subtree")
	_assert(_recon.find_by_key(_host, "hero-title") == null, "removed children are unreachable through keys")

	# --- Phase 6: insert ------------------------------------------------------
	print("== insert ==")
	var doc5 := doc4.duplicate(true)
	(doc5["children"][1]["children"] as Array).append(
			{"type": "button", "key": "btn-more", "props": {"text": "More"}})
	stats = _recon.update(_host, doc5)
	_assert(int(stats["created"]) == 1, "insert creates exactly one node")
	_assert(int(stats["reused"]) == DOC_TOTAL_NODES - 3, "existing siblings reused while inserting")
	var more_btn := _recon.find_by_key(_host, "btn-more") as Button
	_assert(more_btn != null and more_btn.text == "More", "inserted node materialized with props")

	# --- Phase 7: same-key type swap is a remount ------------------------------
	print("== type swap ==")
	var old_label := _recon.find_by_key(_host, "status") as Control
	var old_label_id := old_label.get_instance_id()  # captured BEFORE the swap: the node is freed below
	var doc6 := doc5.duplicate(true)
	var status_doc: Dictionary = doc6["children"][0]
	status_doc["type"] = "button"
	(status_doc["props"] as Dictionary)["text"] = "Status: online"
	stats = _recon.update(_host, doc6)
	var status_btn := _recon.find_by_key(_host, "status") as Button
	_assert(not is_instance_valid(old_label), "old text node freed on type change")
	_assert(status_btn is Button and status_btn.get_instance_id() != old_label_id, "same key now backs a Button (new identity)")
	_assert(status_btn.text == "Status: online", "new node received its props")
	_assert(int(stats["created"]) == 1 and int(stats["removed"]) == 1, "type swap books one create and one remove (c=%d r=%d)" % [int(stats["created"]), int(stats["removed"])])
	stats = _recon.update(_host, doc6)
	_assert(int(stats["reused"]) == _recon.node_count(_host) and int(stats["created"]) == 0 and int(stats["removed"]) == 0 and int(stats["patched"]) == 0,
			"replaying an identical document is a pure reuse no-op (reused=%d)" % int(stats["reused"]))

	# --- Phase 8: event wiring ------------------------------------------------
	print("== events ==")
	_recon.action.connect(_on_action)
	var send2 := _recon.find_by_key(_host, "btn-send") as Button
	send2.pressed.emit()
	_assert(_action_log == ["send_msg"], "String on_click dispatched through reconciler.action")
	# Patch btn-stop's handler from a String id to a real Callable and verify
	# the diff rewires the signal (exactly one application).
	var doc6b := doc6.duplicate(true)
	var stop_doc: Dictionary = doc6b["children"][1]["children"][1]
	stop_doc["props"]["on_click"] = Callable(self, "_on_plain_callback")
	stats = _recon.update(_host, doc6b)
	_assert(int(stats["patched"]) == 1, "Callable on_click diff counts as one patch")
	var stop_btn := _recon.find_by_key(_host, "btn-stop") as Button
	stop_btn.pressed.emit()
	_assert(_cb_hit, "Callable on_click connected and fired")

	# --- Phase 9: root identity swap + unknown types --------------------------
	print("== root swap / unknown types ==")
	var prev_root := _host.get_child(0) as Control
	var doc8 := {
		"type": "row", "key": "gen2",
		"props": {},
		"children": [
			{"type": "text", "key": "solo", "props": {"text": "solo"}},
			{"type": "mystery-widget", "key": "bad", "props": {}},
		],
	}
	stats = _recon.update(_host, doc8)
	_assert(int(stats["removed"]) == 6, "root identity swap frees the whole previous tree (removed=%d)" % int(stats["removed"]))
	_assert(int(stats["created"]) == 2, "replacement root + valid child created, unknown type skipped")
	_assert(int(stats["skipped"]) == 1, "unknown type counted as skipped, not fatal")
	_assert(_recon.find_by_key(_host, "solo") is Label, "new root type materializes")
	_assert(is_instance_valid(prev_root) == false, "previous root tree freed on identity change")

	# --- Phase 10: unmount frees everything -----------------------------------
	print("== unmount ==")
	var live := _live_ids(_host)
	_assert(live.size() == 2, "engine subtree holds 2 nodes before detach")
	var freed: int = _recon.detach(_host)
	_assert(freed == live.size(), "detach frees every engine node at once")
	_assert(_host.get_child_count() == 0, "host child count is zero after detach")
	_assert(_recon.node_count(_host) == 0, "node_count reads zero after detach")
	var all_freed := true
	for ref in live:
		if is_instance_valid(ref):
			all_freed = false
	_assert(all_freed, "every detached node reference is invalid after free")

	stats = _recon.update(_host, _fresh_doc())
	_assert(int(stats["created"]) == DOC_TOTAL_NODES, "remount after detach rebuilds the full tree")

	# --- Phase 11: layout engine modes ----------------------------------------
	print("== layout engine ==")
	var checks := [
		[1920.0, "WIDE"], [1280.0, "WIDE"], [1279.0, "COMPACT"],
		[1024.0, "COMPACT"], [1023.0, "NARROW"], [640.0, "NARROW"],
		[639.0, "TINY"], [320.0, "TINY"],
	]
	for c in checks:
		_assert(String(_eng.mode_name(_eng.mode_for_width(float(c[0])))) == String(c[1]),
				"%.0fpx resolves %s" % [float(c[0]), String(c[1])])
	var wide_rules: Dictionary = _eng.rules_for_width(1440.0)
	var compact_rules: Dictionary = _eng.rules_for_width(1024.0)
	var narrow_rules: Dictionary = _eng.rules_for_width(900.0)
	var tiny_rules: Dictionary = _eng.rules_for_width(480.0)
	_assert(int(wide_rules["columns"]) == 3 and bool(wide_rules["sidebar_visible"]) and bool(wide_rules["details_visible"]),
			"WIDE exposes sidebar and details")
	_assert(int(wide_rules["gap"]) > int(compact_rules["gap"]) and int(compact_rules["gap"]) > int(narrow_rules["gap"]) and int(narrow_rules["gap"]) > int(tiny_rules["gap"]),
			"stack gap contracts as width shrinks")
	_assert(int(wide_rules["padding"]) > int(tiny_rules["padding"]), "padding contracts at TINY")
	_assert(float(tiny_rules["font_scale"]) < 1.0, "TINY scales typography down")
	_assert(bool(compact_rules["sidebar_visible"]) and not bool(compact_rules["details_visible"]), "COMPACT hides details only")
	_assert(not bool(narrow_rules["sidebar_visible"]), "NARROW hides the sidebar")
	var tuned: DshLayoutT = DshLayoutT.new()
	tuned.configure(1000.0, 800.0, 400.0)
	_assert(tuned.mode_for_width(900.0) == DshLayoutT.LayoutMode.COMPACT, "custom breakpoints re-tier 900px to COMPACT")
	_assert(tuned.mode_for_width(1000.0) == DshLayoutT.LayoutMode.WIDE, "custom WIDE breakpoint is inclusive at 1000")
	_assert(tuned.mode_for_width(999.0) == DshLayoutT.LayoutMode.COMPACT, "custom breakpoints keep COMPACT just under 1000")
	_assert(DshLayoutT.default_mode_for_width(900.0) == DshLayoutT.LayoutMode.NARROW, "static default contract works without an instance")

	# --- Phase 12: viewport-driven layout --------------------------------------
	print("== viewport integration ==")
	get_viewport().size = Vector2i(1440, 900)
	await _wait_frames(2)
	_assert(String(_eng.rules_for_viewport(get_viewport())["mode_name"]) == "WIDE", "1440px viewport resolves WIDE")
	get_viewport().size = Vector2i(900, 700)
	await _wait_frames(2)
	_assert(String(_eng.rules_for_viewport(get_viewport())["mode_name"]) == "NARROW", "900px viewport resolves NARROW")

	print("")
	print("UI_ENGINE_RESULT passed=%d failed=%d" % [_passed, _failed])
	get_tree().quit(1 if _failed > 0 else 0)