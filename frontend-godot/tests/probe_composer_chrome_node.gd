extends Node

var _passed := 0
var _failed := 0


func _ready() -> void:
	await get_tree().process_frame
	await _run()
	print("COMPOSER_CHROME_RESULT passed=%d failed=%d" % [_passed, _failed])
	get_tree().quit(1 if _failed > 0 else 0)


func _run() -> void:
	var packed: PackedScene = load("res://scenes/chrome/composer.tscn")
	_assert(packed != null, "composer.tscn loads")
	if packed == null:
		return
	var bar: Control = packed.instantiate()
	_assert(bar != null, "composer instantiates")
	add_child(bar)
	await get_tree().process_frame
	await get_tree().process_frame

	var access := bar.get_node_or_null("%AccessBtn") as Button
	var reject := bar.get_node_or_null("%RejectAllBtn") as Button
	var model_btn := bar.get_node_or_null("%ModelEffortBtn") as Button
	var overflow := bar.get_node_or_null("%OverflowBtn") as Button
	var send := bar.get_node_or_null("%SendBtn") as Button
	var left := bar.get_node_or_null("%LeftChrome") as Control
	var prompt := bar.get_node_or_null("%Prompt") as Control
	_assert(access != null, "AccessBtn exists")
	_assert(reject != null, "RejectAllBtn exists")
	_assert(model_btn != null, "ModelEffortBtn exists")
	_assert(overflow != null, "OverflowBtn exists")
	_assert(send != null, "SendBtn exists")
	_assert(left != null and access != null and left.is_ancestor_of(access), "approval chip is under LeftChrome")
	_assert(prompt != null, "Prompt exists")
	if bar.has_method("is_reject_all"):
		_assert(bar.has_method("set_reject_all"), "set_reject_all API")
		_assert(bar.has_method("set_effort"), "set_effort API")
		_assert(bar.has_method("current_effort"), "current_effort API")
		_assert(bar.has_method("is_generating"), "is_generating API")
		bar.call("set_effort", "low")
		_assert(str(bar.call("current_effort")) == "low", "effort roundtrip")
		bar.call("set_reject_all", true)
		_assert(bool(bar.call("is_reject_all")) == true, "reject_all roundtrip")
	if model_btn != null:
		_assert(model_btn.text.find("·") >= 0 or model_btn.text.strip_edges() != "", "model+effort button has a label")
	if bar.has_signal("effort_changed") and bar.has_signal("reject_all_toggled"):
		_assert(true, "habit-chain signals exist")
	else:
		_assert(false, "effort_changed / reject_all_toggled signals")

	_assert(bar.has_method("current_access_mode"), "current_access_mode exists")
	if bar.has_method("set_access_mode") and bar.has_method("current_access_mode"):
		bar.call("set_access_mode", "accept-edits")
		_assert(str(bar.call("current_access_mode")) == "accept-edits", "set_access_mode accept-edits roundtrip")
	else:
		_assert(false, "set_access_mode accept-edits roundtrip")
	_assert(bar.has_method("reload_chrome"), "reload_chrome exists")
	_assert(bar.has_signal("chrome_customize_requested"), "chrome_customize_requested signal exists")
	if bar.has_method("reload_chrome"):
		bar.call("reload_chrome")
		await get_tree().process_frame
	_assert(left != null and access != null and left.is_ancestor_of(access), "AccessBtn still under LeftChrome")
	if model_btn != null:
		_assert(model_btn.text.find("·") >= 0, "ModelEffortBtn still shows ·")

	# --- W10-c additions: engine factory provenance + reconciler node reuse ------
	# (a) %Prompt must be the engine factory product (text_input -> TextEdit with
	#     reconciler metas), not a surviving legacy scene node.
	var engine_prompt := bar.get_node_or_null("%Prompt") as Control
	_assert(engine_prompt is TextEdit, "%Prompt is a TextEdit (engine text_input product)")
	_assert(engine_prompt != null and engine_prompt.has_meta("_dsh_type")
			and str(engine_prompt.get_meta("_dsh_type")) == "text_input",
			"%Prompt carries _dsh_type==\"text_input\" (engine factory, not scene node)")
	_assert(engine_prompt != null and str(engine_prompt.get_meta("_dsh_key", "")) == "prompt",
			"%Prompt carries _dsh_key==\"prompt\" (document-keyed mount)")
	# (b) apply_tokens twice must reuse the live node (keyed reconciler, no rebuild).
	var reuse_prompt := bar.get_node_or_null("%Prompt") as TextEdit
	if reuse_prompt != null:
		var prompt_id := reuse_prompt.get_instance_id()
		bar.call("apply_tokens")
		bar.call("apply_tokens")
		await get_tree().process_frame
		var reused := bar.get_node_or_null("%Prompt") as Control
		_assert(reused != null and reused.get_instance_id() == prompt_id,
				"apply_tokens x2 reuses the %Prompt instance (instance id unchanged)")
		# (c) reload_chrome must not remount the prompt either.
		bar.call("reload_chrome")
		await get_tree().process_frame
		await get_tree().process_frame
		var still := bar.get_node_or_null("%Prompt") as Control
		_assert(still != null and still.get_instance_id() == prompt_id,
				"reload_chrome still reuses the %Prompt instance")
	else:
		_assert(false, "apply_tokens x2 reuses the %Prompt instance (node missing)")
		_assert(false, "reload_chrome still reuses the %Prompt instance (node missing)")

	# --- D2 additions: slash command palette (engine cmd_palette) data->visible --
	# 契约：set_commands 喂候选 → set_draft("/") 必须让引擎实体化的 %CmdPalette
	# 可见且≥1 项；"/perm" 前缀过滤仍显示 permission；离开 "/" 语境隐藏。
	bar.call("set_commands", [
		{"name": "help", "description": "x"},
		{"name": "permission", "description": "y"},
	])
	bar.call("set_draft", "/")
	await get_tree().process_frame
	await get_tree().process_frame
	var pal := bar.get_node_or_null("%CmdPalette") as ItemList
	_assert(pal != null, "%CmdPalette resolves (engine cmd_palette bound to _cmd_list)")
	if pal != null:
		_assert(pal.visible, "set_draft('/') shows the cmd palette")
		_assert(pal.item_count >= 1, "cmd palette lists >=1 candidate (item_count=%d)" % pal.item_count)
		var prompt_d2 := bar.get_node_or_null("%Prompt") as Control
		var pal_parent := pal.get_parent()
		_assert(pal_parent != null and prompt_d2 != null and pal_parent == prompt_d2.get_parent()
				and pal.get_index() == prompt_d2.get_index() - 1,
				"palette sits in prompt's parent column directly above prompt")
		bar.call("set_draft", "/perm")
		await get_tree().process_frame
		_assert(pal.visible and pal.item_count >= 1,
				"'/perm' prefix keeps palette with matching candidate (items=%d)" % pal.item_count)
		bar.call("set_draft", "plain text")
		await get_tree().process_frame
		_assert(not pal.visible, "non-slash draft hides the palette")

	# --- D1 additions: chrome TEXT chips must actually render their label -------
	# 文案本身从未断过（_apply_strings 一直写入）；D1 是几何缺陷：clip_text=true
	# 且无 min_width 时 Godot 会把文本宽度从 Button 最小尺寸中剔除，内容矩形
	# 0 宽 → 像素上空标签。两半都钉住：非空文案 + 几何容纳（clip 关闭、宽度
	# >= 实际排版的文案宽度）。
	await get_tree().process_frame
	await get_tree().process_frame
	var d1_text := access != null and access.text.strip_edges() != "" \
			and reject != null and reject.text.strip_edges() != "" \
			and model_btn != null and model_btn.text.contains("·")
	var d1_geo := access != null and reject != null and model_btn != null \
			and not access.clip_text and not reject.clip_text and not model_btn.clip_text \
			and access.size.x >= dsh_label_width(access) and reject.size.x >= dsh_label_width(reject) \
			and model_btn.size.x >= dsh_label_width(model_btn)
	_assert(d1_text and d1_geo, "D1: chip labels render (non-empty text + label-fitting geometry)")

	# --- IME 全角斜杠前缀归一（中文优先产品的身份级修复，supremacy-plan §1） ------
	# 中文 IME 无法直出半角 "/"，起手指令实际落在 顿号(、U+3001)/全角斜杠
	# (／U+FF0F) 键位。composer 在 draft 层把首字符一次性原位改写为 "/"
	# （幂等、caret 保位，见 composer.gd _normalize_slash_prefix；，(U+FF0C)
	# 非斜杠键位不参与），命令面板行为必须与手敲 "/" 完全一致。
	bar.call("set_draft", "、help")
	await get_tree().process_frame
	var ime_pal := bar.get_node_or_null("%CmdPalette") as ItemList
	var ime_draft_ok := str(bar.call("get_draft")).begins_with("/help")
	var ime_cand_ok := false
	if ime_pal != null and ime_pal.visible:
		for i in ime_pal.item_count:
			if str(ime_pal.get_item_text(i)).begins_with("/help"):
				ime_cand_ok = true
				break
	_assert(ime_draft_ok and ime_cand_ok,
			"set_draft('、help') → draft 原位归一为 '/help' 且面板列出 /help 候选")
	bar.call("set_draft", "／perm")
	await get_tree().process_frame
	var fw_pal := bar.get_node_or_null("%CmdPalette") as ItemList
	var fw_narrowed := str(bar.call("get_draft")).begins_with("/perm") \
			and fw_pal != null and fw_pal.visible and fw_pal.item_count == 1 \
			and str(fw_pal.get_item_text(0)).begins_with("/permission")
	_assert(fw_narrowed,
			"set_draft('／perm') → draft 归一为 '/perm' 且面板窄化为唯一 /permission 候选")


## Shaped label width of a Button under its live theme font/size.
func dsh_label_width(btn: Button) -> float:
	if btn == null or btn.text.strip_edges() == "":
		return 0.0
	return btn.get_theme_font("font").get_string_size(
			btn.text, HORIZONTAL_ALIGNMENT_LEFT, -1, btn.get_theme_font_size("font_size")).x


func _assert(cond: bool, msg: String) -> void:
	if cond:
		_passed += 1
		print("  [PASS] " + msg)
	else:
		_failed += 1
		print("  [FAIL] " + msg)
