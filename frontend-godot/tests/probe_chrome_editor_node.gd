extends Node

## Headless probe for the engine-driven chrome editor overlay.
##
## Instantiates DshChromeEditor (scripts/ui/chrome/editor.gd) in the tree and
## checks the public API plus the engine mount path:
##   open / slots / save(layout_saved) / reset(approval) / close / is_open
##
## Verdict line:
##     CHROME_EDITOR_RESULT passed=<P> failed=<F>

const DshReconT := preload("res://engine/reconciler.gd")
const DshChromeDocT := preload("res://documents/chrome_editor_doc.gd")
const EditorT := preload("res://scripts/ui/chrome/editor.gd")
const LayoutT := preload("res://scripts/ui/chrome/layout.gd")

const LAYOUT_PATH := "user://chrome_layout.json"

var _passed := 0
var _failed := 0
var _motion_was := true
var _layout_backup := ""
var _layout_existed := false


func _assert(cond: bool, msg: String) -> void:
	if cond:
		_passed += 1
		print("  PASS: ", msg)
	else:
		_failed += 1
		print("  FAIL: ", msg)


func _frames(n: int) -> void:
	for _i in n:
		await get_tree().process_frame


func _find_keyed(node: Node, key: String) -> Control:
	if node is Control:
		var ctl := node as Control
		if ctl.has_meta(DshReconT.META_KEY) and str(ctl.get_meta(DshReconT.META_KEY)) == key:
			return ctl
	for child in node.get_children():
		var hit := _find_keyed(child, key)
		if hit != null:
			return hit
	return null


func _ready() -> void:
	await _run()


func _run() -> void:
	_motion_was = DshTokens.motion_enabled
	DshTokens.motion_enabled = false
	_layout_existed = FileAccess.file_exists(LAYOUT_PATH)
	if _layout_existed:
		_layout_backup = FileAccess.get_file_as_string(LAYOUT_PATH)

	print("== document ==")
	var doc: Dictionary = DshChromeDocT.build({
		"slots": LayoutT.DEFAULT.duplicate(true),
		"palette": [],
	})
	_assert(not doc.is_empty(), "chrome_editor_doc.build() returned an AST")
	_assert(DshReconT.validate(doc).is_empty(), "AST passes reconciler validate()")
	_assert(str(doc.get("type", "")) == "column", "root is a column")
	_assert(str(doc.get("key", "")) == "chrome-root", "root key is chrome-root")
	var seen := {}
	var dup := _collect_keys(doc, seen)
	_assert(not dup, "document keys are unique")
	_assert(seen.has("chrome-slot-left") and seen.has("chrome-palette"),
			"document keys include slot rows and palette")
	_assert(seen.has("chrome-save") and seen.has("chrome-reset") and seen.has("chrome-close"),
			"document keys include save / reset / close")

	print("== api ==")
	var editor = EditorT.new()
	_assert(editor != null, "DshChromeEditor.new() constructs without a scene")
	add_child(editor)
	await _frames(2)
	_assert(not editor.is_open(), "is_open is false before open()")

	editor.open()
	await _frames(4)
	_assert(editor.visible, "open() makes visible")
	_assert(editor.is_open(), "is_open is true after open()")

	var slots: Dictionary = editor.current_layout()
	_assert(slots.has("composer.left"), "layout slots include composer.left")
	_assert(slots.has("composer.right") and slots.has("composer.overflow"),
			"layout slots include right and overflow")

	print("== engine mount ==")
	var use_engine := bool(editor.get("_use_engine"))
	_assert(use_engine, "engine path stays enabled (_use_engine)")
	var host: Control = editor.get("_host") as Control
	_assert(host != null, "inner host Control exists")
	var host_child: Node = host.get_child(0) if host != null and host.get_child_count() > 0 else null
	_assert(host_child is VBoxContainer, "host child type is VBoxContainer (engine column)")
	_assert(host != null and host.has_meta(DshReconT.META_ROOT), "recon.update stored META_ROOT on host")
	var stats: Dictionary = editor.get("_last_stats")
	_assert(int(stats.get("created", 0)) > 0, "recon.update called (created > 0)")
	_assert(_find_keyed(editor, "chrome-slot-left") != null, "engine tree has chrome-slot-left")
	var save_btn := _find_keyed(editor, "chrome-save") as Button
	_assert(save_btn != null, "engine tree has chrome-save button")
	var reset_btn := _find_keyed(editor, "chrome-reset") as Button
	_assert(reset_btn != null, "engine tree has chrome-reset button")
	_assert(_find_keyed(editor, "chrome-close") != null, "engine tree has chrome-close button")

	# Seed a known default so later move/reset asserts don't depend on leftover JSON.
	if reset_btn != null:
		reset_btn.pressed.emit()
	await _frames(3)

	print("== save ==")
	var saved: Array = []
	editor.connect("layout_saved", func(data: Dictionary) -> void:
		saved.append(data)
	)
	save_btn = _find_keyed(editor, "chrome-save") as Button
	if save_btn != null:
		save_btn.pressed.emit()
	await _frames(2)
	_assert(saved.size() == 1, "save emits layout_saved")
	_assert(saved.size() > 0 and (saved[0] as Dictionary).has("composer.left"),
			"layout_saved payload includes composer.left")

	print("== reset ==")
	var move_out := _find_keyed(editor, "chrome-move-left-approval-out") as Button
	_assert(move_out != null, "move-out control exists for approval on left")
	if move_out != null:
		move_out.pressed.emit()
	await _frames(3)
	var after_move: Dictionary = editor.current_layout()
	var left_after: Array = after_move.get("composer.left", [])
	_assert(left_after.find("approval") < 0, "moving approval out of left works without drag")
	reset_btn = _find_keyed(editor, "chrome-reset") as Button
	if reset_btn != null:
		reset_btn.pressed.emit()
	await _frames(3)
	var after_reset: Dictionary = editor.current_layout()
	var left_reset: Array = after_reset.get("composer.left", [])
	_assert(left_reset.find("approval") >= 0, "reset restores default left widgets containing approval")

	print("== close / is_open roundtrip ==")
	editor.close()
	await _frames(2)
	_assert(not editor.visible, "close hides")
	_assert(not editor.is_open(), "is_open is false after close()")
	editor.open()
	await _frames(3)
	_assert(editor.is_open() and editor.visible, "open() after close() is visible")
	editor.close()
	await _frames(1)
	_assert(not editor.is_open(), "is_open roundtrip ends closed")

	var engine_child := "none"
	if host != null and host.get_child_count() > 0:
		engine_child = host.get_child(0).get_class()
	print("CHROME_EDITOR_ENGINE mounted=%s host_child=%s recon_created=%s" % [
		str(use_engine and host_child is VBoxContainer).to_lower(),
		engine_child,
		str(int(stats.get("created", 0))),
	])

	_restore()
	print("")
	print("CHROME_EDITOR_RESULT passed=%d failed=%d" % [_passed, _failed])
	get_tree().quit(1 if _failed > 0 else 0)


func _restore() -> void:
	DshTokens.motion_enabled = _motion_was
	if _layout_existed:
		var f := FileAccess.open(LAYOUT_PATH, FileAccess.WRITE)
		if f != null:
			f.store_string(_layout_backup)
	elif FileAccess.file_exists(LAYOUT_PATH):
		DirAccess.remove_absolute(ProjectSettings.globalize_path(LAYOUT_PATH))


func _collect_keys(node: Dictionary, seen: Dictionary) -> bool:
	if node.is_empty():
		return false
	var key := str(node.get("key", ""))
	if key != "":
		if seen.has(key):
			return true
		seen[key] = true
	for child in node.get("children", []):
		if child is Dictionary and _collect_keys(child as Dictionary, seen):
			return true
	return false
