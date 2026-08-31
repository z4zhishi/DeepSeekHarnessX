extends Node

## Headless runtime probe for the W10-c engine-mounted composer.
##
## Drives the REAL runtime path — scenes/chrome/composer.tscn instantiated
## fresh; its script (composer.gd) discards the legacy static scene subtree and
## mounts documents/composer_doc.gd:build_mount_doc() through DshUIReconciler
## plus the registered widget factories. Proves Goal §W10-c acceptance in
## behavior (no screenshots):
##   1. factory product — %Prompt/%SendBtn/%AccessBtn resolve to
##      reconciler-materialized nodes (META_TYPE/META_KEY metas), not the
##      legacy scene children
##   2. reuse           — apply_tokens twice + reload_chrome keep the same
##      instance ids (keyed reconciliation, not Dictionary equality)
##   3. IA + API        — AccessBtn under LeftChrome, access/effort roundtrips,
##      "model · effort" label, reload_chrome exists and is callable
##   6. behaviors       — prompt_submitted / command_submitted / stop_requested /
##      reject_all_toggled captured from real button presses
##
## chrome_customize_requested is only asserted to exist: its sole emitter sits
## behind the interactive overflow tray popover (covered by the GUI chrome
## editor flow), so there is no public script trigger for it.
##
## Verdict line (grep this in CI):
##     PROBE_COMPOSER_RUNTIME_RESULT passed=<p> failed=<f>

const DshReconT := preload("res://engine/reconciler.gd")
const REJECT_PATH := "user://approval_auto_reject.txt"

var _passed := 0
var _failed := 0
var _reject_had_file := false
var _reject_backup := ""


func _ready() -> void:
	await get_tree().process_frame
	await _run()
	_restore_reject_file()
	print("PROBE_COMPOSER_RUNTIME_RESULT passed=%d failed=%d" % [_passed, _failed])
	get_tree().quit(1 if _failed > 0 else 0)


func _run() -> void:
	# The reject-all toggle persists to user:// on real toggles; restore the
	# user's original file state when the probe finishes.
	_reject_had_file = FileAccess.file_exists(REJECT_PATH)
	if _reject_had_file:
		var f := FileAccess.open(REJECT_PATH, FileAccess.READ)
		_reject_backup = f.get_as_text() if f != null else ""

	var packed: PackedScene = load("res://scenes/chrome/composer.tscn")
	_assert(packed != null, "composer.tscn loads")
	if packed == null:
		return
	var bar: Control = packed.instantiate()
	_assert(bar != null, "composer instantiates")
	add_child(bar)
	await get_tree().process_frame
	await get_tree().process_frame

	# --- signal contract --------------------------------------------------------
	print("== signal contract ==")
	for s in ["prompt_submitted", "stop_requested", "command_submitted", "model_selected",
			"access_mode_requested", "effort_changed", "reject_all_toggled", "chrome_customize_requested"]:
		_assert(bar.has_signal(s), "composer declares signal %s" % s)

	# --- condition 1: engine factory product ------------------------------------
	print("== condition 1: engine factory product ==")
	var prompt := bar.get_node_or_null("%Prompt")
	var typed_prompt := prompt as TextEdit
	_assert(typed_prompt != null, "%Prompt resolves and is a TextEdit (engine text_input product)")
	_assert(typed_prompt != null and typed_prompt.has_meta(DshReconT.META_TYPE)
			and str(typed_prompt.get_meta(DshReconT.META_TYPE)) == "text_input",
			"%Prompt carries _dsh_type==\"text_input\" (reconciler factory, not the scene node)")
	_assert(typed_prompt != null and typed_prompt.has_meta(DshReconT.META_KEY)
			and str(typed_prompt.get_meta(DshReconT.META_KEY)) == "prompt",
			"%Prompt carries _dsh_key==\"prompt\" (document-keyed mount)")
	var send := bar.get_node_or_null("%SendBtn") as Button
	_assert(send != null and str(send.get_meta(DshReconT.META_KEY, "")) == "send_button",
			"%SendBtn is an engine Button keyed send_button")
	var access := bar.get_node_or_null("%AccessBtn") as Button
	_assert(access != null and str(access.get_meta(DshReconT.META_KEY, "")) == "access_chip",
			"%AccessBtn is an engine Button keyed access_chip")
	var mount_root: Variant = bar.get_meta(DshReconT.META_ROOT) if bar.has_meta(DshReconT.META_ROOT) else null
	_assert(mount_root is Control
			and str((mount_root as Control).get_meta(DshReconT.META_KEY, "")) == "composer_stack",
			"mounted engine root keyed composer_stack sits under the composer")

	# --- condition 2: instance-id reuse -----------------------------------------
	print("== condition 2: instance-id reuse (keyed reconciler, not rebuild) ==")
	if typed_prompt == null:
		_assert(false, "apply_tokens x2 keeps the %Prompt instance (no node to check)")
		_assert(false, "reload_chrome keeps the %Prompt instance (no node to check)")
	else:
		var prompt_id := typed_prompt.get_instance_id()
		var send_id := send.get_instance_id() if send != null else -1
		var access_id := access.get_instance_id() if access != null else -1
		bar.call("apply_tokens")
		await get_tree().process_frame
		var after_1 := bar.get_node_or_null("%Prompt") as Control
		_assert(after_1 != null and after_1.get_instance_id() == prompt_id,
				"apply_tokens #1 keeps the %Prompt instance")
		bar.call("apply_tokens")
		await get_tree().process_frame
		var after_2 := bar.get_node_or_null("%Prompt") as Control
		_assert(after_2 != null and after_2.get_instance_id() == prompt_id,
				"apply_tokens #2 keeps the %Prompt instance")
		bar.call("reload_chrome")
		await get_tree().process_frame
		await get_tree().process_frame
		var after_reload := bar.get_node_or_null("%Prompt") as Control
		_assert(after_reload != null and after_reload.get_instance_id() == prompt_id,
				"reload_chrome keeps the %Prompt instance")
		var send_after := bar.get_node_or_null("%SendBtn") as Button
		var access_after := bar.get_node_or_null("%AccessBtn") as Button
		_assert(send != null and send_id >= 0 and send.get_instance_id() == send_id
				and (send_after as Control) == (send as Control),
				"reload_chrome keeps the %SendBtn instance")
		_assert(access != null and access_id >= 0 and access.get_instance_id() == access_id
				and (access_after as Control) == (access as Control),
				"reload_chrome keeps the %AccessBtn instance")

	# --- condition 3: chrome IA + behavior API ----------------------------------
	print("== condition 3: chrome IA + behavior API roundtrips ==")
	var left := bar.get_node_or_null("%LeftChrome") as Control
	_assert(left != null and access != null and left.is_ancestor_of(access),
			"AccessBtn sits under LeftChrome")
	var access_box: Array = []
	bar.access_mode_requested.connect(func(preset: String) -> void:
		access_box.append(preset))
	bar.call("set_access_mode", "accept-edits")
	_assert(str(bar.call("current_access_mode")) == "accept-edits",
			"set_access_mode(\"accept-edits\") -> current_access_mode()")
	_assert(access_box.is_empty(), "set_access_mode syncs silently (no access_mode_requested)")
	var effort_box: Array = []
	bar.effort_changed.connect(func(effort: String) -> void:
		effort_box.append(effort))
	bar.call("set_effort", "low")
	_assert(str(bar.call("current_effort")) == "low", "set_effort(\"low\") -> current_effort()")
	_assert(effort_box.is_empty(), "set_effort syncs silently (no effort_changed)")
	var model_btn := bar.get_node_or_null("%ModelEffortBtn") as Button
	_assert(model_btn != null and model_btn.text.find("·") >= 0,
			"ModelEffortBtn keeps the \"model · effort\" label")
	_assert(bar.has_method("reload_chrome"), "reload_chrome API exists")
	bar.call("reload_chrome")
	await get_tree().process_frame
	_assert(is_instance_valid(left) and bar.get_node_or_null("%AccessBtn") is Button
			and left.is_ancestor_of(bar.get_node_or_null("%AccessBtn")),
			"reload_chrome keeps AccessBtn wired under LeftChrome")

	# --- condition 6: script-triggered behaviors ---------------------------------
	print("== condition 6: script-triggered behaviors ==")
	if send == null:
		_assert(false, "SendBtn available for submit behavior probes")
	else:
		# a. prompt path: draft -> Send press -> prompt_submitted(text, attachments)
		var prompt_box: Array = []
		bar.prompt_submitted.connect(func(text: String, attachments: Array) -> void:
			prompt_box.append([text, attachments.duplicate()]))
		bar.call("set_draft", "hello engine")
		_assert(str(bar.call("get_draft")) == "hello engine",
				"set_draft/get_draft roundtrip on the engine TextEdit")
		send.pressed.emit()
		var prompt_hit: Variant = (prompt_box[0] as Array) if prompt_box.size() == 1 else []
		var got_text: Variant = (prompt_hit as Array)[0] if prompt_hit is Array and (prompt_hit as Array).size() > 0 else null
		var got_atts: Variant = (prompt_hit as Array)[1] if prompt_hit is Array and (prompt_hit as Array).size() > 1 else null
		_assert(got_text == "hello engine",
				"Send press routes the draft through prompt_submitted(\"hello engine\")")
		_assert(got_atts is Array and (got_atts as Array).is_empty(),
				"prompt_submitted carries an empty attachments list for a plain draft")
		_assert(str(bar.call("get_draft")) == "", "draft is cleared after submit")

		# b. command path: slash draft -> Send press -> command_submitted(line)
		var cmd_box: Array = []
		bar.command_submitted.connect(func(line: String) -> void:
			cmd_box.append(line))
		bar.call("set_draft", "/plan on")
		send.pressed.emit()
		_assert(cmd_box == ["/plan on"], "slash draft routes through command_submitted(\"/plan on\")")
		_assert(prompt_box.size() == 1, "command submit does not double-fire prompt_submitted")

		# c. stop path: generating Send -> stop_requested (never submit)
		var stop_box: Array = []
		bar.stop_requested.connect(func() -> void:
			stop_box.append(true))
		bar.call("set_generating", true)
		_assert(bool(bar.call("is_generating")) == true, "set_generating(true) roundtrip")
		send.pressed.emit()
		_assert(stop_box.size() == 1, "Send while generating emits stop_requested")
		_assert(prompt_box.size() == 1 and cmd_box.size() == 1,
				"generating press submits nothing (stop semantics replace submit)")
		bar.call("set_generating", false)
		_assert(bool(bar.call("is_generating")) == false, "set_generating(false) roundtrip")

		# d. chrome_customize_requested — interactive-only emitter (overflow tray),
		#    covered by the GUI chrome editor flow; existence checked only.
		_assert(bar.has_signal("chrome_customize_requested"),
				"chrome_customize_requested exists (customize popover is interactive-only, not script-triggered)")

	# e. reject-all: internal sync stays silent, a real toggle emits
	var reject := bar.get_node_or_null("%RejectAllBtn") as Button
	if reject == null:
		_assert(false, "RejectAllBtn available for reject_all_toggled probe")
	else:
		var reject_box: Array = []
		bar.reject_all_toggled.connect(func(enabled: bool) -> void:
			reject_box.append(enabled))
		bar.call("set_reject_all", true)
		_assert(bool(bar.call("is_reject_all")) == true and reject_box.is_empty(),
				"set_reject_all(true) rounds the state silently (set_pressed_no_signal path)")
		reject.set_pressed(false)
		_assert(reject_box == [false] and not bool(bar.call("is_reject_all")),
				"real toggle press emits reject_all_toggled(false) and flips is_reject_all")


func _restore_reject_file() -> void:
	if _reject_had_file:
		var w := FileAccess.open(REJECT_PATH, FileAccess.WRITE)
		if w != null:
			w.store_string(_reject_backup)
	elif FileAccess.file_exists(REJECT_PATH):
		DirAccess.remove_absolute(REJECT_PATH)


func _assert(cond: bool, msg: String) -> void:
	if cond:
		_passed += 1
		print("  [PASS] " + msg)
	else:
		_failed += 1
		print("  [FAIL] " + msg)