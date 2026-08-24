extends CanvasLayer
class_name SettingsOverlay

## Settings panel: appearance, language, models, provider profiles.

signal theme_changed(is_dark: bool)
signal closed

const THEME_PATH := "user://theme.txt"
const PANEL_SIZE := Vector2(720, 560)
const PROTOCOLS := [
	["openai-completions", "provider.protoCompletions", "OpenAI Completions"],
	["openai-responses", "provider.protoResponses", "OpenAI Responses"],
	["anthropic-messages", "provider.protoAnthropic", "Anthropic Messages"],
]

var _client: DshClient = null
var _profiles: Array = []
var _active_profile: String = ""
var _editing_id: String = ""
var _seeded_default := false

var _backdrop: ColorRect
var _card: PanelContainer
var _title: Label
var _status: Label
var _dark_btn: Button
var _light_btn: Button
var _lang_zh: Button
var _lang_en: Button
var _model_opt: OptionButton
var _profile_list: ItemList
var _proto_opt: OptionButton
var _base_edit: LineEdit
var _model_edit: LineEdit
var _key_edit: LineEdit
var _name_lbl: Label
var _save_btn: Button
var _active_btn: Button
var _add_btn: Button
var _del_btn: Button
var _appearance_lbl: Label
var _language_lbl: Label
var _model_lbl: Label
var _provider_lbl: Label
var _proto_lbl: Label
var _base_lbl: Label
var _pmodel_lbl: Label
var _key_lbl: Label
var _close_btn: Button


func _ready() -> void:
	layer = 21
	visible = false
	_build()
	if get_node_or_null("/root/DshI18n") != null:
		DshI18n.locale_changed.connect(_on_locale)


func setup(client: DshClient) -> void:
	_client = client


func open() -> void:
	visible = true
	_apply_style()
	_apply_strings()
	_sync_appearance()
	_sync_language()
	_load_models()
	_load_profiles()


func close() -> void:
	visible = false
	closed.emit()


func _build() -> void:
	_backdrop = ColorRect.new()
	_backdrop.color = Color(0, 0, 0, 0.5)
	_backdrop.set_anchors_preset(Control.PRESET_FULL_RECT)
	_backdrop.mouse_filter = Control.MOUSE_FILTER_STOP
	_backdrop.gui_input.connect(_on_backdrop)
	add_child(_backdrop)

	var center := CenterContainer.new()
	center.set_anchors_preset(Control.PRESET_FULL_RECT)
	center.mouse_filter = Control.MOUSE_FILTER_IGNORE
	add_child(center)

	_card = PanelContainer.new()
	_card.custom_minimum_size = PANEL_SIZE
	center.add_child(_card)

	var root := VBoxContainer.new()
	root.add_theme_constant_override("separation", 12)
	_card.add_child(root)

	var header := HBoxContainer.new()
	root.add_child(header)
	_title = Label.new()
	_title.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_title.add_theme_font_size_override("font_size", 18)
	header.add_child(_title)
	_close_btn = Button.new()
	_close_btn.text = "×"
	_close_btn.custom_minimum_size = Vector2(32, 28)
	_close_btn.pressed.connect(close)
	header.add_child(_close_btn)

	var scroll := ScrollContainer.new()
	scroll.size_flags_vertical = Control.SIZE_EXPAND_FILL
	scroll.horizontal_scroll_mode = ScrollContainer.SCROLL_MODE_DISABLED
	root.add_child(scroll)

	var body := VBoxContainer.new()
	body.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	body.custom_minimum_size = Vector2(PANEL_SIZE.x - 56.0, 0.0)
	body.add_theme_constant_override("separation", 14)
	scroll.add_child(body)

	_appearance_lbl = _section_label(body)
	var theme_row := HBoxContainer.new()
	theme_row.add_theme_constant_override("separation", 8)
	body.add_child(theme_row)
	_dark_btn = Button.new()
	_dark_btn.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_dark_btn.pressed.connect(_set_dark.bind(true))
	theme_row.add_child(_dark_btn)
	_light_btn = Button.new()
	_light_btn.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_light_btn.pressed.connect(_set_dark.bind(false))
	theme_row.add_child(_light_btn)

	_language_lbl = _section_label(body)
	var lang_row := HBoxContainer.new()
	lang_row.add_theme_constant_override("separation", 8)
	body.add_child(lang_row)
	_lang_zh = Button.new()
	_lang_zh.text = "中文"
	_lang_zh.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_lang_zh.pressed.connect(_on_lang.bind("zh"))
	lang_row.add_child(_lang_zh)
	_lang_en = Button.new()
	_lang_en.text = "English"
	_lang_en.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_lang_en.pressed.connect(_on_lang.bind("en"))
	lang_row.add_child(_lang_en)

	_model_lbl = _section_label(body)
	_model_opt = OptionButton.new()
	_model_opt.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_model_opt.item_selected.connect(_on_model_selected)
	body.add_child(_model_opt)

	_provider_lbl = _section_label(body)
	var split := HBoxContainer.new()
	split.size_flags_vertical = Control.SIZE_EXPAND_FILL
	split.add_theme_constant_override("separation", 12)
	body.add_child(split)

	var left := VBoxContainer.new()
	left.custom_minimum_size = Vector2(200, 180)
	split.add_child(left)
	_profile_list = ItemList.new()
	_profile_list.size_flags_vertical = Control.SIZE_EXPAND_FILL
	_profile_list.item_selected.connect(_on_profile_selected)
	left.add_child(_profile_list)
	_add_btn = Button.new()
	_add_btn.pressed.connect(_add_profile)
	left.add_child(_add_btn)

	var right := VBoxContainer.new()
	right.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	right.add_theme_constant_override("separation", 8)
	split.add_child(right)

	_name_lbl = Label.new()
	right.add_child(_name_lbl)

	_proto_opt = OptionButton.new()
	_proto_lbl = Label.new()
	right.add_child(_field_row(_proto_lbl, _proto_opt))

	_base_edit = LineEdit.new()
	_base_lbl = Label.new()
	right.add_child(_field_row(_base_lbl, _base_edit))
	_model_edit = LineEdit.new()
	_pmodel_lbl = Label.new()
	right.add_child(_field_row(_pmodel_lbl, _model_edit))
	_key_edit = LineEdit.new()
	_key_edit.secret = true
	_key_lbl = Label.new()
	right.add_child(_field_row(_key_lbl, _key_edit))

	var ops := HBoxContainer.new()
	ops.add_theme_constant_override("separation", 8)
	right.add_child(ops)
	_save_btn = Button.new()
	_save_btn.pressed.connect(_save_profile)
	ops.add_child(_save_btn)
	_active_btn = Button.new()
	_active_btn.pressed.connect(_set_profile_active)
	ops.add_child(_active_btn)
	_del_btn = Button.new()
	_del_btn.pressed.connect(_delete_profile)
	ops.add_child(_del_btn)

	_status = Label.new()
	_status.autowrap_mode = TextServer.AUTOWRAP_WORD_SMART
	root.add_child(_status)

	_fill_protocol()
	_apply_style()
	_apply_strings()


func _section_label(parent: VBoxContainer) -> Label:
	var lbl := Label.new()
	lbl.add_theme_font_size_override("font_size", 13)
	parent.add_child(lbl)
	return lbl


func _field_row(lbl: Label, control: Control) -> HBoxContainer:
	var row := HBoxContainer.new()
	row.add_theme_constant_override("separation", 8)
	lbl.custom_minimum_size = Vector2(96, 0)
	lbl.add_theme_font_size_override("font_size", 13)
	row.add_child(lbl)
	control.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	row.add_child(control)
	return row


func _fill_protocol() -> void:
	var current := _protocol_value() if _proto_opt.item_count > 0 else "openai-completions"
	_proto_opt.clear()
	for i in PROTOCOLS.size():
		var row: Array = PROTOCOLS[i]
		_proto_opt.add_item(_t(str(row[1]), str(row[2])))
		_proto_opt.set_item_metadata(i, row[0])
	_select_protocol(current)


func _select_protocol(proto: String) -> void:
	var mapped := _map_protocol(proto)
	for i in _proto_opt.item_count:
		if str(_proto_opt.get_item_metadata(i)) == mapped:
			_proto_opt.select(i)
			return
	if _proto_opt.item_count > 0:
		_proto_opt.select(0)


func _map_protocol(proto: String) -> String:
	match proto:
		"openai-responses":
			return "openai-responses"
		"anthropic-messages":
			return "anthropic-messages"
		_:
			return "openai-completions"


func _t(key: String, fallback: String) -> String:
	var v := str(DshI18n.t(key))
	return fallback if v == key or v.strip_edges() == "" else v


func _apply_style() -> void:
	_card.add_theme_stylebox_override("panel", DshTokens.box(
		DshTokens.bg_layer1(),
		DshTokens.RADIUS_LG,
		DshTokens.border_l2(),
		1,
		Vector4(22, 18, 22, 18)
	))
	_title.add_theme_color_override("font_color", DshTokens.text_primary())
	_status.add_theme_color_override("font_color", DshTokens.text_tertiary())
	for lbl in [_appearance_lbl, _language_lbl, _model_lbl, _provider_lbl, _name_lbl, _proto_lbl, _base_lbl, _pmodel_lbl, _key_lbl]:
		if lbl != null:
			lbl.add_theme_color_override("font_color", DshTokens.text_secondary())
	_profile_list.add_theme_stylebox_override("panel", DshTokens.box(
		DshTokens.bg_layer2(),
		DshTokens.RADIUS_MD,
		DshTokens.border_l1(),
		1,
		Vector4(4, 4, 4, 4)
	))


func _apply_strings() -> void:
	_title.text = DshI18n.t("settings.title")
	_appearance_lbl.text = DshI18n.t("settings.appearance")
	_language_lbl.text = DshI18n.t("settings.language")
	_model_lbl.text = DshI18n.t("settings.model")
	_provider_lbl.text = DshI18n.t("provider.profiles")
	_dark_btn.text = DshI18n.t("app.themeDark")
	_light_btn.text = DshI18n.t("app.themeLight")
	_save_btn.text = DshI18n.t("common.save")
	_active_btn.text = DshI18n.t("provider.setActive")
	_add_btn.text = DshI18n.t("provider.addProfile")
	_del_btn.text = DshI18n.t("provider.removeProfile")
	_key_edit.placeholder_text = DshI18n.t("provider.apiKeyPlaceholder")
	_fill_protocol()
	_proto_lbl.text = DshI18n.t("provider.protocol")
	_base_lbl.text = DshI18n.t("provider.baseUrl")
	_pmodel_lbl.text = DshI18n.t("provider.model")
	_key_lbl.text = DshI18n.t("provider.apiKey")
	_base_edit.placeholder_text = DshI18n.t("provider.baseUrl")
	_model_edit.placeholder_text = DshI18n.t("provider.model")


func _on_locale(_loc: String) -> void:
	_apply_strings()
	_rebuild_profile_list()
	_render_profile_detail()


func _sync_appearance() -> void:
	var dark: bool = DshTokens.is_dark()
	_dark_btn.disabled = dark
	_light_btn.disabled = not dark


func _sync_language() -> void:
	var zh := DshI18n.get_locale() == "zh"
	_lang_zh.disabled = zh
	_lang_en.disabled = not zh


func _set_dark(dark: bool) -> void:
	DshTokens.mode = DshTokens.Mode.DARK if dark else DshTokens.Mode.LIGHT
	var f := FileAccess.open(THEME_PATH, FileAccess.WRITE)
	if f != null:
		f.store_string("dark" if dark else "light")
		f.close()
	_apply_style()
	_sync_appearance()
	theme_changed.emit(dark)


func _on_lang(loc: String) -> void:
	DshI18n.set_locale(loc)
	_sync_language()
	if _client != null:
		_client.settings_mutate("general", [{"op": "set", "path": ["language"], "value": loc}], Callable())


func _load_models() -> void:
	_model_opt.clear()
	if _client == null:
		return
	if _client.has_method("provider_models"):
		_client.provider_models("", _on_provider_models)
	else:
		_client.list_models(_on_models)


func _on_provider_models(ok: bool, data: Variant) -> void:
	if ok and data is Dictionary:
		var models: Variant = (data as Dictionary).get("models", [])
		if models is Array and not (models as Array).is_empty():
			_on_models(true, data)
			return
	if _client != null:
		_client.list_models(_on_models)


func _on_models(ok: bool, data: Variant) -> void:
	_model_opt.clear()
	if not (ok and data is Dictionary):
		return
	var d: Dictionary = data
	var selected := str(d.get("selected", d.get("active", "")))
	var models: Variant = d.get("models", [])
	if not (models is Array):
		return
	var i := 0
	for m in models:
		if not (m is Dictionary):
			continue
		var md: Dictionary = m
		var id := str(md.get("id", ""))
		_model_opt.add_item(str(md.get("name", id)))
		_model_opt.set_item_metadata(i, id)
		if id == selected:
			_model_opt.select(i)
		i += 1


func _on_model_selected(index: int) -> void:
	if _client == null:
		return
	var id := str(_model_opt.get_item_metadata(index))
	if id == "":
		return
	_client.set_model(id, Callable())


func _load_profiles() -> void:
	if _client == null:
		return
	_client.provider_describe(_on_describe)


func _profiles_from(data: Variant) -> Array:
	if data is Array:
		return data
	if data is Dictionary:
		var out: Array = []
		var src: Dictionary = data
		for k in src.keys():
			var v: Variant = src[k]
			if v is Dictionary:
				var pd: Dictionary = (v as Dictionary).duplicate()
				if str(pd.get("id", "")) == "":
					pd["id"] = str(k)
				out.append(pd)
		return out
	return []


func _on_describe(ok: bool, data: Variant) -> void:
	if not (ok and data is Dictionary):
		_status.text = DshI18n.t("provider.noActive")
		_seed_deepseek_if_empty()
		return
	var d: Dictionary = data
	_profiles = _profiles_from(d.get("profiles", []))
	_active_profile = str(d.get("active", ""))
	if _profiles.is_empty():
		_seed_deepseek_if_empty()
		return
	_rebuild_profile_list()
	_render_profile_detail()
	if bool(d.get("usable", false)):
		_load_models()


func _seed_deepseek_if_empty() -> void:
	if _seeded_default or _client == null or not _profiles.is_empty():
		return
	_seeded_default = true
	_status.text = DshI18n.t("provider.noActive")
	_client.provider_set({
		"id": "deepseek",
		"name": "DeepSeek",
		"protocol": "openai-completions",
		"baseUrl": "https://api.deepseek.com",
		"model": "deepseek-v4-flash",
		"setActive": true,
	}, _on_saved)


func _rebuild_profile_list() -> void:
	_profile_list.clear()
	for i in _profiles.size():
		var p: Variant = _profiles[i]
		if not (p is Dictionary):
			continue
		var d: Dictionary = p
		var id := str(d.get("id", ""))
		var label := str(d.get("name", id))
		if id == _active_profile:
			label += "  ●"
		_profile_list.add_item(label)
		_profile_list.set_item_metadata(_profile_list.item_count - 1, i)
		if id == _editing_id or (_editing_id == "" and id == _active_profile):
			_profile_list.select(_profile_list.item_count - 1)
			_editing_id = id


func _current_profile() -> Dictionary:
	var sel := _profile_list.get_selected_items()
	if sel.is_empty():
		if _profiles.size() > 0 and _profiles[0] is Dictionary:
			return _profiles[0]
		return {}
	var idx: int = int(_profile_list.get_item_metadata(sel[0]))
	if idx >= 0 and idx < _profiles.size() and _profiles[idx] is Dictionary:
		return _profiles[idx]
	return {}


func _on_profile_selected(_index: int) -> void:
	var p := _current_profile()
	_editing_id = str(p.get("id", ""))
	_render_profile_detail()


func _render_profile_detail() -> void:
	var p := _current_profile()
	if p.is_empty():
		_name_lbl.text = DshI18n.t("provider.noActive")
		_base_edit.text = ""
		_model_edit.text = ""
		_key_edit.text = ""
		return
	var id := str(p.get("id", ""))
	_editing_id = id
	_name_lbl.text = DshI18n.t("provider.name") + ": " + str(p.get("name", id))
	_select_protocol(str(p.get("protocol", "openai-completions")))
	_base_edit.text = str(p.get("baseUrl", ""))
	_model_edit.text = str(p.get("model", ""))
	_key_edit.text = ""
	if bool(p.get("keyConfigured", false)):
		_key_edit.placeholder_text = DshI18n.t("provider.apiKeyConfigured")
	else:
		_key_edit.placeholder_text = DshI18n.t("provider.apiKeyPlaceholder")


func _protocol_value() -> String:
	if _proto_opt.item_count == 0 or _proto_opt.selected < 0:
		return "openai-completions"
	var meta: Variant = _proto_opt.get_item_metadata(_proto_opt.selected)
	return _map_protocol(str(meta))


func _save_profile() -> void:
	if _client == null:
		return
	var p := _current_profile()
	var id := str(p.get("id", _editing_id))
	if id == "":
		return
	var payload := {
		"id": id,
		"name": str(p.get("name", id)),
		"protocol": _protocol_value(),
		"baseUrl": _base_edit.text.strip_edges(),
		"model": _model_edit.text.strip_edges(),
		"setActive": false,
	}
	var key := _key_edit.text.strip_edges()
	if key != "":
		payload["apiKey"] = key
	_client.provider_set(payload, _on_saved)


func _set_profile_active() -> void:
	if _client == null:
		return
	var p := _current_profile()
	var id := str(p.get("id", _editing_id))
	if id == "":
		return
	_client.provider_set({"id": id, "setActive": true}, _on_saved)


func _on_saved(ok: bool, _data: Variant) -> void:
	_status.text = DshI18n.t("provider.saved") if ok else DshI18n.t("provider.testFailed")
	if ok:
		_load_profiles()
		_load_models()


func _add_profile() -> void:
	if _client == null:
		return
	var id := "profile-" + str(_profiles.size() + 1)
	_client.provider_set({
		"id": id,
		"name": id,
		"protocol": "openai-completions",
		"baseUrl": "https://api.deepseek.com",
		"model": "deepseek-v4-flash",
		"setActive": false,
	}, _on_saved)


func _delete_profile() -> void:
	if _client == null:
		return
	var p := _current_profile()
	var id := str(p.get("id", _editing_id))
	if id == "":
		return
	_client.provider_delete(id, _on_saved)


func _on_backdrop(event: InputEvent) -> void:
	if event is InputEventMouseButton:
		var mb := event as InputEventMouseButton
		if mb.pressed and mb.button_index == MOUSE_BUTTON_LEFT:
			close()


func _unhandled_input(event: InputEvent) -> void:
	if not visible:
		return
	if event is InputEventKey:
		var k := event as InputEventKey
		if k.pressed and not k.echo and k.keycode == KEY_ESCAPE:
			close()
			get_viewport().set_input_as_handled()
