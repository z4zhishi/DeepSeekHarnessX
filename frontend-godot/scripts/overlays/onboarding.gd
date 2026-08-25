extends CanvasLayer
class_name OnboardingOverlay

## First-run: language → provider key (saved) → models. Skip allowed.

signal onboarding_completed
signal onboarding_skipped

var _client: DshClient = null
var _step: int = 1
var _protocol: String = "openai-completions"
var _base_url: String = "https://api.deepseek.com"
var _api_key: String = ""
var _profile_name: String = "default"
var _model: String = "deepseek-v4-flash"
var _models: Array = []
var _fetching: bool = false
var _saving: bool = false

var _backdrop: ColorRect
var _card: PanelContainer
var _title: Label
var _desc: Label
var _content: VBoxContainer
var _footer: HBoxContainer
var _name_edit: LineEdit
var _base_edit: LineEdit
var _key_edit: LineEdit
var _proto_opt: OptionButton
var _model_opt: OptionButton
var _model_direct: LineEdit


func _ready() -> void:
	layer = 22
	visible = false
	_build()
	if get_node_or_null("/root/DshI18n") != null:
		DshI18n.locale_changed.connect(_on_locale)


func setup(client: DshClient) -> void:
	_client = client


func maybe_start(client: DshClient) -> void:
	setup(client)
	if client == null:
		return
	client.provider_describe(func(ok: bool, data: Variant) -> void:
		if not (ok and data is Dictionary):
			return
		var d: Dictionary = data
		_profiles_from(d.get("profiles", []))
		if not bool(d.get("usable", true)):
			open()
	)


func open() -> void:
	_step = 1
	visible = true
	_fade_open()
	_render_step()


func close() -> void:
	_fade_close()


## §4 minimal motion: quick opacity fade on open/close, layout never animated.
func _fade_open() -> void:
	DshTokens.fade_in(_backdrop, DshTokens.MOTION_BASE)
	DshTokens.fade_in(_card, DshTokens.MOTION_BASE)


func _fade_close() -> void:
	DshTokens.fade_out(_card, DshTokens.MOTION_QUICK, func() -> void: visible = false)
	if _backdrop != null:
		DshTokens.fade_out(_backdrop, DshTokens.MOTION_QUICK, Callable())


func _build() -> void:
	_backdrop = ColorRect.new()
	_backdrop.color = Color(0, 0, 0, 0.55)
	_backdrop.set_anchors_preset(Control.PRESET_FULL_RECT)
	_backdrop.mouse_filter = Control.MOUSE_FILTER_STOP
	add_child(_backdrop)

	var center := CenterContainer.new()
	center.set_anchors_preset(Control.PRESET_FULL_RECT)
	center.mouse_filter = Control.MOUSE_FILTER_IGNORE
	add_child(center)

	_card = PanelContainer.new()
	_card.custom_minimum_size = Vector2(540, 440)
	center.add_child(_card)

	var box := VBoxContainer.new()
	box.add_theme_constant_override("separation", 14)
	_card.add_child(box)

	_title = Label.new()
	_title.add_theme_font_size_override("font_size", 22)
	_title.horizontal_alignment = HORIZONTAL_ALIGNMENT_CENTER
	box.add_child(_title)

	_desc = Label.new()
	_desc.add_theme_font_size_override("font_size", 13)
	_desc.horizontal_alignment = HORIZONTAL_ALIGNMENT_CENTER
	_desc.autowrap_mode = TextServer.AUTOWRAP_WORD_SMART
	box.add_child(_desc)

	_content = VBoxContainer.new()
	_content.size_flags_vertical = Control.SIZE_EXPAND_FILL
	_content.add_theme_constant_override("separation", 10)
	box.add_child(_content)

	box.add_child(HSeparator.new())

	_footer = HBoxContainer.new()
	_footer.add_theme_constant_override("separation", 10)
	_footer.alignment = BoxContainer.ALIGNMENT_END
	box.add_child(_footer)

	_apply_style()


func _apply_style() -> void:
	_card.add_theme_stylebox_override("panel", DshTokens.box(
		DshTokens.bg_layer2(),
		DshTokens.RADIUS_LG,
		DshTokens.border_l2(),
		1,
		Vector4(28, 24, 28, 24)
	))
	_title.add_theme_color_override("font_color", DshTokens.text_primary())
	_desc.add_theme_color_override("font_color", DshTokens.text_tertiary())


func _on_locale(_loc: String) -> void:
	if visible:
		_render_step()


func _clear_box(box: Control) -> void:
	for c in box.get_children():
		box.remove_child(c)
		c.queue_free()


func _render_step() -> void:
	_apply_style()
	_clear_box(_content)
	_clear_box(_footer)
	_name_edit = null
	_base_edit = null
	_key_edit = null
	_proto_opt = null
	_model_opt = null
	_model_direct = null
	if _step == 1:
		_render_language()
	elif _step == 2:
		_render_provider()
	else:
		_render_model()


func _render_language() -> void:
	_title.text = DshI18n.t("onboarding.welcomeTitle")
	_desc.text = DshI18n.t("onboarding.languagePrompt")
	var grid := GridContainer.new()
	grid.columns = 2
	grid.add_theme_constant_override("h_separation", 12)
	grid.add_theme_constant_override("v_separation", 12)
	_content.add_child(grid)
	_lang_button(grid, "中文", "zh")
	_lang_button(grid, "English", "en")
	_skip_button()


func _lang_button(parent: Control, label: String, loc: String) -> void:
	var b := Button.new()
	b.custom_minimum_size = Vector2(180, 56)
	b.text = label
	b.add_theme_font_size_override("font_size", 18)
	b.pressed.connect(_pick_language.bind(loc))
	parent.add_child(b)


func _pick_language(loc: String) -> void:
	DshI18n.set_locale(loc)
	if _client != null:
		_client.settings_mutate("general", [{"op": "set", "path": ["language"], "value": loc}], Callable())
	_step = 2
	_render_step()


func _render_provider() -> void:
	_title.text = DshI18n.t("onboarding.providerTitle")
	_desc.text = DshI18n.t("onboarding.providerDesc")
	_name_edit = _add_field(DshI18n.t("provider.name"), _profile_name)
	_name_edit.text_changed.connect(_on_name_changed)
	_proto_opt = OptionButton.new()
	_proto_opt.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	var protos := [
		["openai-completions", "provider.protoCompletions", "OpenAI Completions"],
		["openai-responses", "provider.protoResponses", "OpenAI Responses"],
		["anthropic-messages", "provider.protoAnthropic", "Anthropic Messages"],
	]
	var pick := 0
	for i in protos.size():
		var row: Array = protos[i]
		_proto_opt.add_item(_ot(str(row[1]), str(row[2])))
		_proto_opt.set_item_metadata(i, row[0])
		if str(row[0]) == _protocol:
			pick = i
	_proto_opt.select(pick)
	_proto_opt.item_selected.connect(_on_protocol)
	_content.add_child(_field_row(DshI18n.t("provider.protocol"), _proto_opt))
	_base_edit = _add_field(DshI18n.t("provider.baseUrl"), _base_url)
	_base_edit.text_changed.connect(_on_base_changed)
	_key_edit = _add_field(DshI18n.t("provider.apiKey"), "")
	_key_edit.secret = true
	_key_edit.placeholder_text = DshI18n.t("provider.apiKeyPlaceholder")
	_key_edit.text_changed.connect(_on_key_changed)
	_footer_btn(DshI18n.t("common.back"), false).pressed.connect(_go_step.bind(1))
	_skip_button()
	var next := _footer_btn(DshI18n.t("common.next"), true)
	next.pressed.connect(_save_key_then_models)


func _on_name_changed(t: String) -> void:
	_profile_name = t


func _on_base_changed(t: String) -> void:
	_base_url = t


func _on_key_changed(t: String) -> void:
	_api_key = t


func _on_protocol(idx: int) -> void:
	var proto := str(_proto_opt.get_item_metadata(idx))
	if proto == "deepseek" or proto == "openai" or proto == "":
		proto = "openai-completions"
	_protocol = proto


func _save_key_then_models() -> void:
	if _saving:
		return
	if _api_key.strip_edges() == "":
		_desc.text = DshI18n.t("provider.keyRequired")
		return
	if _client == null:
		_step = 3
		_render_step()
		return
	_saving = true
	_desc.text = DshI18n.t("common.loading")
	var id := _profile_name.strip_edges()
	if id == "":
		id = "default"
	_client.provider_set({
		"id": id,
		"name": id,
		"protocol": _protocol,
		"baseUrl": _base_url.strip_edges(),
		"model": _model,
		"apiKey": _api_key.strip_edges(),
		"setActive": true,
	}, _on_provider_saved)


func _on_provider_saved(ok: bool, _data: Variant) -> void:
	_saving = false
	if not ok:
		_desc.text = DshI18n.t("provider.testFailed")
		return
	_step = 3
	_render_step()
	_fetch_models()


func _render_model() -> void:
	_title.text = DshI18n.t("onboarding.modelTitle")
	_desc.text = DshI18n.t("onboarding.modelDesc")
	_model_opt = OptionButton.new()
	_model_opt.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_fill_model_opt()
	_model_opt.item_selected.connect(_on_model_picked)
	_content.add_child(_model_opt)
	var or_lbl := Label.new()
	or_lbl.text = DshI18n.t("provider.modelName")
	or_lbl.add_theme_font_size_override("font_size", 12)
	or_lbl.add_theme_color_override("font_color", DshTokens.text_tertiary())
	_content.add_child(or_lbl)
	_model_direct = LineEdit.new()
	_model_direct.placeholder_text = DshI18n.t("provider.model")
	_model_direct.text_changed.connect(_on_model_direct)
	_content.add_child(_model_direct)
	_footer_btn(DshI18n.t("common.back"), false).pressed.connect(_go_step.bind(2))
	var finish := _footer_btn(DshI18n.t("onboarding.finish"), true)
	finish.pressed.connect(_finish)


func _fill_model_opt() -> void:
	if _model_opt == null:
		return
	_model_opt.clear()
	if _models.is_empty():
		_model_opt.add_item(_model)
		_model_opt.set_item_metadata(0, _model)
		return
	var i := 0
	for m in _models:
		if not (m is Dictionary):
			continue
		var d: Dictionary = m
		var id := str(d.get("id", ""))
		_model_opt.add_item(str(d.get("name", id)))
		_model_opt.set_item_metadata(i, id)
		if id == _model:
			_model_opt.select(i)
		i += 1


func _on_model_picked(idx: int) -> void:
	_model = str(_model_opt.get_item_metadata(idx))


func _on_model_direct(t: String) -> void:
	if t.strip_edges() != "":
		_model = t.strip_edges()


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


func _fetch_models() -> void:
	if _fetching or _client == null:
		return
	_fetching = true
	_desc.text = DshI18n.t("settings.fetchingModels")
	if _client.has_method("provider_models"):
		_client.provider_models("", _on_models)
	elif _client.has_method("list_models"):
		_client.list_models(_on_models)
	else:
		_fetching = false
		_on_models(false, {})


func _on_models(ok: bool, data: Variant) -> void:
	_fetching = false
	var failed := not ok
	if ok and data is Dictionary:
		var d: Dictionary = data
		var raw: Variant = d.get("models", [])
		if raw is Array:
			_models = raw
		elif raw is Dictionary:
			_models = _profiles_from(raw)
		if str(d.get("fetchError", "")) != "":
			failed = true
		var sel := str(d.get("selected", ""))
		if sel != "":
			_model = sel
	if _models.is_empty() or failed:
		_models = [
			{"id": "deepseek-v4-flash", "name": "DeepSeek V4 Flash"},
			{"id": "deepseek-chat", "name": "DeepSeek Chat"},
			{"id": "deepseek-reasoner", "name": "DeepSeek Reasoner"},
		]
		_desc.text = DshI18n.t("settings.modelFetchFailed")
	else:
		_desc.text = DshI18n.t("onboarding.modelDesc")
	_fill_model_opt()


func _finish() -> void:
	if _client != null and _model != "":
		_client.set_model(_model, _on_finish_model)
	else:
		_emit_completed()


func _on_finish_model(_ok: bool, _data: Variant) -> void:
	_emit_completed()


func _emit_completed() -> void:
	_fade_close()
	onboarding_completed.emit()


func _go_step(step: int) -> void:
	_step = step
	_render_step()


func _skip_button() -> void:
	var skip := _footer_btn(DshI18n.t("onboarding.skip"), false)
	skip.pressed.connect(_skip)


func _skip() -> void:
	_fade_close()
	onboarding_skipped.emit()


func _add_field(label: String, initial: String) -> LineEdit:
	var edit := LineEdit.new()
	edit.text = initial
	edit.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_content.add_child(_field_row(label, edit))
	return edit


func _ot(key: String, fallback: String) -> String:
	var v := str(DshI18n.t(key))
	return fallback if v == key or v.strip_edges() == "" else v


func _field_row(label: String, control: Control) -> HBoxContainer:
	var row := HBoxContainer.new()
	row.add_theme_constant_override("separation", 8)
	var lbl := Label.new()
	lbl.text = label
	lbl.custom_minimum_size = Vector2(110, 0)
	lbl.add_theme_font_size_override("font_size", 13)
	lbl.add_theme_color_override("font_color", DshTokens.text_secondary())
	row.add_child(lbl)
	control.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	row.add_child(control)
	return row


func _footer_btn(text: String, primary: bool) -> Button:
	var b := Button.new()
	b.text = text
	b.custom_minimum_size = Vector2(100, 34)
	if primary:
		b.add_theme_stylebox_override("normal", DshTokens.box(
			DshTokens.brand_button(),
			DshTokens.RADIUS_MD,
			Color(0, 0, 0, 0),
			0,
			Vector4(12, 6, 12, 6)
		))
		b.add_theme_color_override("font_color", DshTokens.bg_base())
		b.add_theme_color_override("font_hover_color", DshTokens.bg_base())
	_footer.add_child(b)
	return b
