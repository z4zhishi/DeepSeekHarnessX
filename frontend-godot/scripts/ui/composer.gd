extends PanelContainer
class_name ComposerBar

signal prompt_submitted(text: String, attachments: Array)
signal stop_requested
signal command_submitted(line: String)
signal model_selected(id: String)
signal access_mode_requested(preset: String)

const ACCESS_PRESETS: PackedStringArray = ["default", "strict", "unrestricted"]

@onready var _gen: Label = %GenStatus
@onready var _queue: HBoxContainer = %QueueRail
@onready var _rail: HBoxContainer = %AttachRail
@onready var _prompt: TextEdit = %Prompt
@onready var _cmd: Button = %CmdBtn
@onready var _access: Button = %AccessBtn
@onready var _models: OptionButton = %ModelPicker
@onready var _attach: Button = %AttachBtn
@onready var _attach_icon: TextureRect = %AttachIcon
@onready var _send: Button = %SendBtn
@onready var _send_icon: TextureRect = %SendIcon

var _generating := false
var _enabled := true
var _attachments: Array[String] = []
var _file_dialog: FileDialog = null
var _commands: Array = []
var _cmd_list: ItemList = null
var _syncing_models := false
var _access_i := 0

func _ready() -> void:
	size_flags_horizontal = Control.SIZE_SHRINK_CENTER
	_prompt.wrap_mode = TextEdit.LINE_WRAPPING_BOUNDARY
	if _prompt.get("scroll_fit_content_height") != null:
		_prompt.scroll_fit_content_height = true
	_prompt.caret_blink = true
	_attach.pressed.connect(_open_picker)
	_send.pressed.connect(_on_send_pressed)
	_cmd.pressed.connect(_on_cmd_pressed)
	_access.pressed.connect(_on_access_pressed)
	_models.item_selected.connect(_on_model_item)
	_prompt.text_changed.connect(_on_text_changed)
	_build_cmd_list()
	var seat := get_parent() as Control
	if seat:
		seat.resized.connect(_cap_width)
	if DshI18n.has_signal("locale_changed"):
		DshI18n.locale_changed.connect(func(_loc: String): _apply_strings())
	get_viewport().files_dropped.connect(_on_files_dropped)
	apply_tokens()
	_apply_strings()
	_grow()
	_refresh_send_state()
	call_deferred("_cap_width")


func _input(event: InputEvent) -> void:
	if event is InputEventKey and event.pressed and not event.echo:
		var k := event as InputEventKey
		if k.keycode == KEY_ESCAPE:
			if _cmd_popup_visible():
				_hide_cmd_popup()
				get_viewport().set_input_as_handled()
				return
			if _generating:
				stop_requested.emit()
				get_viewport().set_input_as_handled()
			return
		if not _prompt.has_focus():
			return
		if _cmd_popup_visible() and (k.keycode == KEY_UP or k.keycode == KEY_DOWN):
			_move_cmd_sel(-1 if k.keycode == KEY_UP else 1)
			get_viewport().set_input_as_handled()
			return
		if k.keycode == KEY_TAB and _cmd_popup_visible():
			_apply_selected_cmd()
			get_viewport().set_input_as_handled()
			return
		if k.keycode == KEY_ENTER or k.keycode == KEY_KP_ENTER:
			if k.shift_pressed and not k.ctrl_pressed and not k.meta_pressed:
				return
			get_viewport().set_input_as_handled()
			if _cmd_popup_visible() and _cmd_list.get_selected_items().size() > 0:
				_apply_selected_cmd()
				return
			_submit()


func apply_tokens() -> void:
	add_theme_stylebox_override("panel", DshTokens.box(
		DshTokens.bg_input(),
		DshTokens.RADIUS_COMPOSER,
		DshTokens.border_l2(),
		1,
		Vector4(12, 10, 12, 8)
	))
	_prompt.add_theme_color_override("font_color", DshTokens.text_primary())
	_prompt.add_theme_color_override("font_placeholder_color", DshTokens.text_tertiary())
	_gen.add_theme_color_override("font_color", DshTokens.text_tertiary())
	_gen.add_theme_font_size_override("font_size", DshTokens.FONT_MICRO)
	_cmd.add_theme_font_size_override("font_size", DshTokens.FONT_CHROME)
	_cmd.add_theme_color_override("font_color", DshTokens.text_secondary())
	_access.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
	_access.add_theme_color_override("font_color", DshTokens.text_secondary())
	DshIcons.apply(_attach_icon, "paperclip", 16.0)
	_refresh_send_icon()
	_paint_round(_cmd)
	_paint_chip(_access)
	_paint_round(_attach)
	_paint_round(_send)
	_apply_strings()


func set_generating(generating: bool) -> void:
	_generating = generating
	_gen.visible = generating
	if generating:
		_gen.text = _t("chat.generating", "Deep diving...")
	else:
		_clear_queue()
	_refresh_send_icon()
	_refresh_send_state()


func is_generating() -> bool:
	return _generating


func set_commands(commands: Array) -> void:
	_commands = commands
	_refresh_cmd_popup()


func set_enabled(enabled: bool) -> void:
	_enabled = enabled
	_prompt.editable = enabled
	_attach.disabled = not enabled
	_cmd.disabled = not enabled
	_access.disabled = not enabled
	_models.disabled = (not enabled) or _models.item_count == 0
	_refresh_send_state()


func set_models(models: Array, selected: String) -> void:
	_syncing_models = true
	_models.clear()
	var pick := 0
	for m in models:
		var id := ""
		var label := ""
		if m is Dictionary:
			id = str(m.get("id", ""))
			label = str(m.get("name", id))
		else:
			id = str(m)
			label = id
		if id == "":
			continue
		_models.add_item(label)
		var idx := _models.item_count - 1
		_models.set_item_metadata(idx, id)
		if id == selected:
			pick = idx
	if _models.item_count > 0:
		_models.select(pick)
		_models.disabled = not _enabled
	else:
		_models.disabled = true
	_syncing_models = false
	_refresh_model_tooltip()


func selected_model() -> String:
	if _models.item_count == 0:
		return ""
	var idx := _models.selected
	if idx < 0:
		return ""
	return str(_models.get_item_metadata(idx))


func grab_input_focus() -> void:
	_prompt.grab_focus()


func get_draft() -> String:
	return _prompt.text


func set_draft(text: String) -> void:
	_prompt.text = text
	_grow()
	_refresh_cmd_popup()
	_refresh_send_state()


func _on_send_pressed() -> void:
	if _generating:
		stop_requested.emit()
		return
	_submit()


func _on_cmd_pressed() -> void:
	if not _enabled:
		return
	_prompt.grab_focus()
	if not _prompt.text.begins_with("/"):
		_prompt.text = "/" + _prompt.text
	_prompt.set_caret_line(0)
	_prompt.set_caret_column(_prompt.get_line(0).length())
	_refresh_cmd_popup()
	_grow()
	_refresh_send_state()


func _on_access_pressed() -> void:
	_access_i = (_access_i + 1) % ACCESS_PRESETS.size()
	_access.text = _access_label(ACCESS_PRESETS[_access_i])
	access_mode_requested.emit(ACCESS_PRESETS[_access_i])


func _on_model_item(index: int) -> void:
	if _syncing_models or index < 0:
		return
	var id := str(_models.get_item_metadata(index))
	_refresh_model_tooltip()
	if id != "":
		model_selected.emit(id)


func _submit() -> void:
	if not _enabled:
		return
	var text := _prompt.text.strip_edges()
	if _generating:
		if text == "":
			return
		_prompt.text = ""
		_grow()
		_hide_cmd_popup()
		_add_queue_chip(text)
		_refresh_send_state()
		if text.begins_with("/"):
			command_submitted.emit(text)
		else:
			prompt_submitted.emit(text, [])
		return
	if text == "" and _attachments.is_empty():
		return
	var paths: Array = []
	for p in _attachments:
		paths.append(p)
	if not paths.is_empty():
		var bits: PackedStringArray = PackedStringArray()
		for p in paths:
			bits.append("@" + str(p))
		if text != "":
			text += "\n"
		text += " ".join(bits)
	_prompt.text = ""
	_clear_attachments()
	_grow()
	_hide_cmd_popup()
	_refresh_send_state()
	if text.begins_with("/"):
		command_submitted.emit(text)
	else:
		prompt_submitted.emit(text, paths)


func _on_text_changed() -> void:
	_grow()
	_refresh_cmd_popup()
	_refresh_send_state()


func _build_cmd_list() -> void:
	_cmd_list = ItemList.new()
	_cmd_list.visible = false
	_cmd_list.custom_minimum_size = Vector2(0, 108)
	_cmd_list.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_cmd_list.item_clicked.connect(func(index: int, _pos: Variant = null, _btn: Variant = 0) -> void:
		_cmd_list.select(index)
		_apply_selected_cmd()
	)
	_cmd_list.item_activated.connect(func(_index: int) -> void:
		_apply_selected_cmd()
	)
	_cmd_list.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
	var box := _prompt.get_parent()
	if box:
		box.add_child(_cmd_list)
		box.move_child(_cmd_list, _prompt.get_index())


func _cmd_popup_visible() -> bool:
	return _cmd_list != null and _cmd_list.visible


func _hide_cmd_popup() -> void:
	if _cmd_list != null:
		_cmd_list.visible = false
		_cmd_list.clear()


func _refresh_cmd_popup() -> void:
	if _cmd_list == null:
		return
	var raw := _prompt.text
	if not raw.begins_with("/") or _commands.is_empty():
		_hide_cmd_popup()
		return
	var rest := raw.substr(1)
	if rest.find(" ") >= 0 or rest.find("\n") >= 0:
		_hide_cmd_popup()
		return
	var needle := rest.strip_edges().to_lower()
	_cmd_list.clear()
	for c in _commands:
		var name := ""
		var desc := ""
		if c is Dictionary:
			name = str((c as Dictionary).get("name", (c as Dictionary).get("id", "")))
			desc = str((c as Dictionary).get("description", (c as Dictionary).get("desc", "")))
		else:
			name = str(c)
		if name.begins_with("/"):
			name = name.substr(1)
		if name == "":
			continue
		if needle != "" and not name.to_lower().begins_with(needle) and name.to_lower().find(needle) < 0:
			continue
		var label := "/" + name
		if desc != "":
			label += "  —  " + desc
		_cmd_list.add_item(label)
		_cmd_list.set_item_metadata(_cmd_list.item_count - 1, name)
	if _cmd_list.item_count == 0:
		_hide_cmd_popup()
		return
	_cmd_list.visible = true
	_cmd_list.select(0)
	_cmd_list.add_theme_stylebox_override("panel", DshTokens.box(
		DshTokens.bg_layer2(),
		DshTokens.RADIUS_MD,
		DshTokens.border_l2(),
		1,
		Vector4(4, 4, 4, 4)
	))


func _move_cmd_sel(delta: int) -> void:
	if _cmd_list == null or _cmd_list.item_count == 0:
		return
	var sel := _cmd_list.get_selected_items()
	var idx := sel[0] if not sel.is_empty() else 0
	idx = wrapi(idx + delta, 0, _cmd_list.item_count)
	_cmd_list.select(idx)
	_cmd_list.ensure_current_is_visible()


func _apply_selected_cmd() -> void:
	if _cmd_list == null:
		return
	var sel := _cmd_list.get_selected_items()
	if sel.is_empty():
		_hide_cmd_popup()
		return
	var name := str(_cmd_list.get_item_metadata(sel[0]))
	_prompt.text = "/" + name + " "
	_prompt.set_caret_line(_prompt.get_line_count() - 1)
	_prompt.set_caret_column(_prompt.get_line(_prompt.get_caret_line()).length())
	_hide_cmd_popup()
	_grow()
	_refresh_send_state()


func _grow() -> void:
	var lines := maxi(_prompt.get_line_count(), 1)
	_prompt.custom_minimum_size.y = clampf(float(lines * DshTokens.FONT_BODY_LH) + 8.0, 44.0, 140.0)


func _cap_width() -> void:
	var seat := get_parent() as Control
	var avail := seat.size.x if seat else DshTokens.COMPOSER_MAX
	custom_minimum_size.x = minf(DshTokens.COMPOSER_MAX, maxf(280.0, avail))


func _refresh_send_icon() -> void:
	DshIcons.apply(_send_icon, "stop" if _generating else "send", 16.0)
	_send.tooltip_text = _t("chat.stopTooltip", "Stop (Esc)") if _generating else _t("chat.sendTooltip", "Send (Enter)")


func _refresh_send_state() -> void:
	if _generating:
		_send.disabled = false
		return
	if not _enabled:
		_send.disabled = true
		return
	_send.disabled = _prompt.text.strip_edges() == "" and _attachments.is_empty()


func _refresh_model_tooltip() -> void:
	if _models.item_count == 0 or _models.selected < 0:
		_models.tooltip_text = _t("common.model", "Select model")
		return
	var cur := _models.get_item_text(_models.selected)
	_models.tooltip_text = "Select model, current %s" % cur


func _access_label(preset: String) -> String:
	match preset:
		"strict":
			return _t("chat.accessRead", "Read only")
		"unrestricted":
			return _t("chat.accessFull", "Full access")
		_:
			return _t("chat.accessWrite", "Workspace Write")


## Queued text is display-only: the message was already steered to the backend,
## so a "remove" affordance would mislead (nothing can actually be recalled).
func _add_queue_chip(text: String) -> void:
	_queue.visible = true
	var wrap := PanelContainer.new()
	wrap.add_theme_stylebox_override("panel", DshTokens.box(
		DshTokens.bg_layer2(),
		DshTokens.RADIUS_PILL,
		DshTokens.border_l1(),
		1,
		Vector4(8, 2, 8, 2)
	))
	var lab := Label.new()
	lab.text = text if text.length() <= 36 else text.substr(0, 33) + "…"
	lab.tooltip_text = text
	lab.mouse_filter = Control.MOUSE_FILTER_PASS
	lab.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
	lab.add_theme_color_override("font_color", DshTokens.text_secondary())
	wrap.add_child(lab)
	DshTokens.fade_in(wrap, DshTokens.MOTION_QUICK)
	_queue.add_child(wrap)


func _refresh_queue_visible() -> void:
	var n := 0
	for c in _queue.get_children():
		if not c.is_queued_for_deletion():
			n += 1
	_queue.visible = n > 0


func _clear_queue() -> void:
	for c in _queue.get_children():
		c.queue_free()
	_queue.visible = false


func _open_picker() -> void:
	if _file_dialog == null:
		_file_dialog = FileDialog.new()
		_file_dialog.file_mode = FileDialog.FILE_MODE_OPEN_FILES
		_file_dialog.access = FileDialog.ACCESS_FILESYSTEM
		_file_dialog.use_native_dialog = true
		_file_dialog.title = _t("chat.attach", "Attach files")
		_file_dialog.files_selected.connect(_on_files_picked)
		_file_dialog.file_selected.connect(func(p: String): _on_files_picked(PackedStringArray([p])))
		add_child(_file_dialog)
	_file_dialog.popup_centered_ratio(0.6)


func _on_files_picked(paths: PackedStringArray) -> void:
	for p in paths:
		_add_attachment(p)


func _on_files_dropped(paths: PackedStringArray) -> void:
	if not _enabled:
		return
	for p in paths:
		_add_attachment(p)


func _add_attachment(path: String) -> void:
	if path == "" or _attachments.has(path):
		return
	_attachments.append(path)
	_rail.visible = true
	var chip := Button.new()
	chip.text = path.get_file()
	chip.tooltip_text = path
	chip.flat = true
	chip.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
	chip.add_theme_color_override("font_color", DshTokens.text_secondary())
	chip.add_theme_stylebox_override("normal", DshTokens.box(DshTokens.bg_layer2(), DshTokens.RADIUS_PILL, DshTokens.border_l1(), 1, Vector4(8, 2, 8, 2)))
	chip.pressed.connect(func(): _remove_attachment(path, chip))
	_rail.add_child(chip)
	_refresh_send_state()


func _remove_attachment(path: String, chip: Button) -> void:
	_attachments.erase(path)
	chip.queue_free()
	if _attachments.is_empty():
		_rail.visible = false
	_refresh_send_state()


func _clear_attachments() -> void:
	_attachments.clear()
	for c in _rail.get_children():
		c.queue_free()
	_rail.visible = false
	_refresh_send_state()


func _paint_round(btn: Button) -> void:
	var pad := Vector4(6, 6, 6, 6)
	btn.add_theme_stylebox_override("normal", DshTokens.box(Color(0, 0, 0, 0), DshTokens.RADIUS_PILL, Color(0, 0, 0, 0), 0, pad))
	btn.add_theme_stylebox_override("hover", DshTokens.box(DshTokens.bg_layer2(), DshTokens.RADIUS_PILL, Color(0, 0, 0, 0), 0, pad))
	btn.add_theme_stylebox_override("pressed", DshTokens.box(DshTokens.bg_layer3(), DshTokens.RADIUS_PILL, Color(0, 0, 0, 0), 0, pad))
	btn.add_theme_stylebox_override("focus", DshTokens.box(Color(0, 0, 0, 0), DshTokens.RADIUS_PILL, DshTokens.accent(), 1, pad))
	btn.flat = true


func _paint_chip(btn: Button) -> void:
	var pad := Vector4(8, 4, 8, 4)
	btn.add_theme_stylebox_override("normal", DshTokens.box(Color(0, 0, 0, 0), DshTokens.RADIUS_MD, Color(0, 0, 0, 0), 0, pad))
	btn.add_theme_stylebox_override("hover", DshTokens.box(DshTokens.bg_layer2(), DshTokens.RADIUS_MD, Color(0, 0, 0, 0), 0, pad))
	btn.add_theme_stylebox_override("pressed", DshTokens.box(DshTokens.bg_layer3(), DshTokens.RADIUS_MD, Color(0, 0, 0, 0), 0, pad))
	btn.add_theme_stylebox_override("focus", DshTokens.box(Color(0, 0, 0, 0), DshTokens.RADIUS_MD, DshTokens.accent(), 1, pad))
	btn.flat = true


func _apply_strings() -> void:
	_prompt.placeholder_text = _t("chat.placeholder", _t("chat.messageAgent", "Message the agent"))
	_attach.tooltip_text = _t("chat.attach", "Attach files (@)")
	_cmd.tooltip_text = _t("chat.commands", "Commands")
	_access.tooltip_text = _t("chat.accessMode", "Access mode")
	_access.text = _access_label(ACCESS_PRESETS[_access_i])
	_gen.text = _t("chat.generating", "Deep diving...")
	_gen.tooltip_text = _t("chat.steerHint", "Ctrl+Enter to steer while generating")
	_refresh_send_icon()
	_refresh_model_tooltip()


func _t(key: String, fallback: String) -> String:
	var v := str(DshI18n.t(key))
	return fallback if v == key or v.strip_edges() == "" else v
