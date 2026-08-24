extends CanvasLayer
class_name PluginsOverlay

## Installed plugins: list, enable, uninstall, install from folder/zip.

var _client: DshClient = null
var _plugins: Array = []
var _selected: String = ""

var _backdrop: ColorRect
var _card: PanelContainer
var _title: Label
var _status: Label
var _list: ItemList
var _empty: Label
var _install_btn: Button
var _enable_btn: Button
var _uninstall_btn: Button
var _dialog: FileDialog = null


func _ready() -> void:
	layer = 31
	visible = false
	_build()
	if get_node_or_null("/root/DshI18n") != null:
		DshI18n.locale_changed.connect(_apply_strings)


func setup(client: DshClient) -> void:
	_client = client


func open_panel() -> void:
	open()


func open() -> void:
	visible = true
	_apply_style()
	_apply_strings()
	_refresh()


func close() -> void:
	visible = false


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
	_card.custom_minimum_size = Vector2(560, 420)
	center.add_child(_card)

	var box := VBoxContainer.new()
	box.add_theme_constant_override("separation", 10)
	_card.add_child(box)

	var header := HBoxContainer.new()
	box.add_child(header)
	_title = Label.new()
	_title.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_title.add_theme_font_size_override("font_size", 16)
	header.add_child(_title)
	var close_btn := Button.new()
	close_btn.text = "×"
	close_btn.custom_minimum_size = Vector2(32, 28)
	close_btn.pressed.connect(close)
	header.add_child(close_btn)

	_list = ItemList.new()
	_list.size_flags_vertical = Control.SIZE_EXPAND_FILL
	_list.custom_minimum_size = Vector2(0, 200)
	_list.item_selected.connect(_on_item_selected)
	box.add_child(_list)

	_empty = Label.new()
	_empty.autowrap_mode = TextServer.AUTOWRAP_WORD_SMART
	box.add_child(_empty)

	_status = Label.new()
	_status.autowrap_mode = TextServer.AUTOWRAP_WORD_SMART
	box.add_child(_status)

	var btns := HBoxContainer.new()
	btns.add_theme_constant_override("separation", 8)
	box.add_child(btns)
	_install_btn = Button.new()
	_install_btn.pressed.connect(_pick_install)
	btns.add_child(_install_btn)
	_enable_btn = Button.new()
	_enable_btn.pressed.connect(_toggle_enabled)
	btns.add_child(_enable_btn)
	_uninstall_btn = Button.new()
	_uninstall_btn.pressed.connect(_uninstall_selected)
	btns.add_child(_uninstall_btn)

	_apply_style()
	_apply_strings()


func _apply_style() -> void:
	_card.add_theme_stylebox_override("panel", DshTokens.box(
		DshTokens.bg_layer1(),
		DshTokens.RADIUS_LG,
		DshTokens.border_l2(),
		1,
		Vector4(18, 16, 18, 16)
	))
	_title.add_theme_color_override("font_color", DshTokens.text_primary())
	_empty.add_theme_color_override("font_color", DshTokens.text_tertiary())
	_status.add_theme_color_override("font_color", DshTokens.text_tertiary())
	_list.add_theme_stylebox_override("panel", DshTokens.box(
		DshTokens.bg_layer2(),
		DshTokens.RADIUS_MD,
		DshTokens.border_l1(),
		1,
		Vector4(4, 4, 4, 4)
	))


func _apply_strings(_loc: String = "") -> void:
	_title.text = _t("plugins.title", "Plugins")
	_install_btn.text = _t("plugins.install", "Install…")
	_enable_btn.text = _t("plugins.enable", "Enable")
	_uninstall_btn.text = _t("plugins.uninstall", "Uninstall")
	_empty.text = _t("plugins.empty", "No plugins installed.")
	_refresh_enable_label()


func _refresh() -> void:
	if _client == null or not _client.has_method("plugin_list"):
		_render([])
		_status.text = _t("plugins.failed", "Plugin service unavailable.")
		return
	_client.plugin_list(_on_listed)


func _on_listed(ok: bool, data: Variant) -> void:
	if not ok:
		_render([])
		_status.text = _rpc_err(data, _t("plugins.failed", "Failed to list plugins."))
		return
	_status.text = ""
	_render(_as_plugins(data))


func _as_plugins(data: Variant) -> Array:
	if data is Array:
		return data
	if data is Dictionary:
		for key in ["plugins", "items", "value"]:
			if (data as Dictionary).get(key) is Array:
				return (data as Dictionary)[key]
	return []


func _render(plugins: Array) -> void:
	_plugins = plugins
	_list.clear()
	_empty.visible = plugins.is_empty()
	var select := -1
	for p in plugins:
		if not (p is Dictionary):
			continue
		var d: Dictionary = p
		var name := str(d.get("name", d.get("id", "")))
		if name == "":
			continue
		var enabled := bool(d.get("enabled", d.get("active", true)))
		var mark := "●" if enabled else "○"
		var extra := str(d.get("path", d.get("kind", "")))
		var label := "%s  %s" % [mark, name]
		if extra != "":
			label += "  —  " + extra
		_list.add_item(label)
		_list.set_item_metadata(_list.item_count - 1, d)
		if name == _selected:
			select = _list.item_count - 1
	if select >= 0:
		_list.select(select)
		_on_item_selected(select)
	else:
		_selected = ""
		_refresh_enable_label()


func _on_item_selected(index: int) -> void:
	var meta: Variant = _list.get_item_metadata(index)
	if meta is Dictionary:
		_selected = str((meta as Dictionary).get("name", (meta as Dictionary).get("id", "")))
	_refresh_enable_label()


func _current() -> Dictionary:
	var sel := _list.get_selected_items()
	if sel.is_empty():
		return {}
	var meta: Variant = _list.get_item_metadata(sel[0])
	return meta if meta is Dictionary else {}


func _refresh_enable_label() -> void:
	var p := _current()
	var enabled := bool(p.get("enabled", p.get("active", true)))
	if p.is_empty():
		_enable_btn.text = _t("plugins.enable", "Enable")
		_enable_btn.disabled = true
		_uninstall_btn.disabled = true
		return
	_enable_btn.disabled = false
	_uninstall_btn.disabled = bool(p.get("builtin", false))
	_enable_btn.text = _t("plugins.disable", "Disable") if enabled else _t("plugins.enable", "Enable")


func _toggle_enabled() -> void:
	var p := _current()
	var name := str(p.get("name", p.get("id", "")))
	if _client == null or name == "":
		return
	var enabled := not bool(p.get("enabled", p.get("active", true)))
	_client.plugin_enable(name, enabled, _on_mutated)


func _uninstall_selected() -> void:
	var p := _current()
	var name := str(p.get("name", p.get("id", "")))
	if _client == null or name == "":
		return
	_client.plugin_uninstall(name, _on_mutated)


func _pick_install() -> void:
	if _dialog == null:
		_dialog = FileDialog.new()
		_dialog.file_mode = FileDialog.FILE_MODE_OPEN_ANY
		_dialog.access = FileDialog.ACCESS_FILESYSTEM
		_dialog.use_native_dialog = true
		_dialog.filters = PackedStringArray(["*.zip ; Zip", "*.json ; Manifest"])
		_dialog.file_selected.connect(_install_path)
		_dialog.dir_selected.connect(_install_path)
		add_child(_dialog)
	_dialog.title = _t("plugins.install", "Install plugin")
	_dialog.popup_centered_ratio(0.7)


func _install_path(path: String) -> void:
	if _client == null or path.strip_edges() == "":
		return
	_client.plugin_install(path.strip_edges(), _on_mutated)


func _on_mutated(ok: bool, data: Variant) -> void:
	if not ok:
		_status.text = _rpc_err(data, _t("plugins.failed", "Plugin action failed."))
	else:
		_status.text = _t("common.saved", "Saved")
	_refresh()


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


func _rpc_err(data: Variant, fallback: String) -> String:
	if data is Dictionary:
		var err: Variant = (data as Dictionary).get("error", "")
		if err is Dictionary:
			var msg := str((err as Dictionary).get("message", ""))
			if msg != "":
				return msg
		var s := str(err).strip_edges()
		if s != "" and s != "<null>":
			return s
	return fallback


func _t(key: String, fallback: String) -> String:
	var v := str(DshI18n.t(key))
	return fallback if v == key or v.strip_edges() == "" else v
