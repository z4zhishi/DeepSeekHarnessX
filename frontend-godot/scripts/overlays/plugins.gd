extends CanvasLayer
class_name PluginsOverlay

## Plugins dashboard driven by the live backend plugin.list registry view.
## List rows carry the real status badge (mounted/installed/disabled/error
## with the error text); the detail pane renders the selected plugin's full
## view — metadata, owned tools/commands, event topics and permission
## defaults. Enable/disable, install and uninstall go through the real
## gateway RPCs and refresh the view after each mutation.

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
var _dialog: DshFilePicker = null
# The live registry view from plugin.list: capabilities, events, permissions.
# Each is a real backend snapshot; the detail pane renders it read-only.
var _capabilities: Array = []
var _events: Array = []
var _permissions: Array = []
var _tool_ownership: Dictionary = {}
var _command_ownership: Dictionary = {}
var _detail_scroll: ScrollContainer
var _detail_box: VBoxContainer


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
	_card.custom_minimum_size = Vector2(780, 520)
	center.add_child(_card)

	var box := VBoxContainer.new()
	box.add_theme_constant_override("separation", 12)
	_card.add_child(box)

	var header := HBoxContainer.new()
	header.add_theme_constant_override("separation", 8)
	box.add_child(header)
	_title = Label.new()
	_title.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_title.add_theme_font_size_override("font_size", DshTokens.FONT_CHROME_LG)
	header.add_child(_title)
	var close_btn := Button.new()
	close_btn.text = "×"
	close_btn.custom_minimum_size = Vector2(32, 28)
	close_btn.pressed.connect(close)
	header.add_child(close_btn)

	var body := HBoxContainer.new()
	body.size_flags_vertical = Control.SIZE_EXPAND_FILL
	body.add_theme_constant_override("separation", 12)
	box.add_child(body)

	var list_col := VBoxContainer.new()
	list_col.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	list_col.add_theme_constant_override("separation", 8)
	body.add_child(list_col)

	_list = ItemList.new()
	_list.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_list.size_flags_vertical = Control.SIZE_EXPAND_FILL
	_list.custom_minimum_size = Vector2(280, 0)
	_list.item_selected.connect(_on_item_selected)
	list_col.add_child(_list)

	_empty = Label.new()
	_empty.autowrap_mode = TextServer.AUTOWRAP_WORD_SMART
	list_col.add_child(_empty)

	_detail_scroll = ScrollContainer.new()
	_detail_scroll.custom_minimum_size = Vector2(330, 0)
	_detail_scroll.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_detail_scroll.size_flags_vertical = Control.SIZE_EXPAND_FILL
	_detail_scroll.horizontal_scroll_mode = ScrollContainer.SCROLL_MODE_DISABLED
	body.add_child(_detail_scroll)

	_detail_box = VBoxContainer.new()
	_detail_box.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_detail_box.add_theme_constant_override("separation", 10)
	_detail_scroll.add_child(_detail_box)

	_status = Label.new()
	_status.autowrap_mode = TextServer.AUTOWRAP_WORD_SMART
	box.add_child(_status)

	var btns := HBoxContainer.new()
	btns.add_theme_constant_override("separation", 8)
	var spring := Control.new()
	spring.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	btns.add_child(spring)
	_install_btn = Button.new()
	_install_btn.pressed.connect(_pick_install)
	btns.add_child(_install_btn)
	_enable_btn = Button.new()
	_enable_btn.pressed.connect(_toggle_enabled)
	btns.add_child(_enable_btn)
	_uninstall_btn = Button.new()
	_uninstall_btn.pressed.connect(_uninstall_selected)
	btns.add_child(_uninstall_btn)
	box.add_child(btns)

	_apply_style()
	_apply_strings()
	# 应用内插件包选择器：常驻预实例化（非原生对话框），首点零冷启动。
	_dialog = DshFilePicker.new()
	_dialog.bucket = "plugin_install"
	_dialog.file_selected.connect(_install_path)
	_dialog.dir_selected.connect(_install_path)
	add_child(_dialog)


func _apply_style() -> void:
	_card.add_theme_stylebox_override("panel", DshTokens.elevated(
		DshTokens.bg_layer1(),
		DshTokens.RADIUS_LG,
		Vector4(18, 16, 18, 16),
		3
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
	_uninstall_btn.text = _t("plugins.uninstall", "Uninstall")
	_empty.text = _t("plugins.empty", "No plugins installed.")
	_refresh_enable_label()
	_render(_plugins)


func _refresh() -> void:
	if _client == null or not _client.has_method("plugin_list"):
		_render([])
		_set_status(_t("plugins.failed", "Plugin service unavailable."), true)
		return
	_client.plugin_list(_on_listed)


func _on_listed(ok: bool, data: Variant) -> void:
	if not ok:
		_render([])
		_set_status(_rpc_err(data, _t("plugins.failed", "Failed to list plugins.")), true)
		return
	_set_status("", false)
	if data is Dictionary:
		var d := data as Dictionary
		_capabilities = d.get("capabilities", []) as Array
		_events = d.get("events", []) as Array
		_permissions = d.get("permissions", []) as Array
		_tool_ownership = d.get("toolOwnership", {}) as Dictionary
		_command_ownership = d.get("commandOwnership", {}) as Dictionary
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
		var pname := str(d.get("name", d.get("id", "")))
		if pname == "":
			continue
		var status := str(d.get("status", ""))
		var line := "%s  %s · %s" % [_status_mark(status), pname, _status_text(status)]
		if status == "error":
			var err_text := str(d.get("error", "")).strip_edges()
			if err_text.length() > 64:
				err_text = err_text.substr(0, 64) + "…"
			if err_text != "":
				line += " — " + err_text
		_list.add_item(line)
		var idx := _list.item_count - 1
		_list.set_item_metadata(idx, d)
		_list.set_item_custom_fg_color(idx, _list_color(status))
		if pname == _selected:
			select = idx
	if select >= 0:
		_list.select(select)
		_on_item_selected(select)
	else:
		_selected = ""
		_render_details("")
		_refresh_enable_label()


func _on_item_selected(index: int) -> void:
	var meta: Variant = _list.get_item_metadata(index)
	var pname := ""
	if meta is Dictionary:
		pname = str((meta as Dictionary).get("name", (meta as Dictionary).get("id", "")))
	_selected = pname
	_render_details(pname)
	_refresh_enable_label()


func _render_details(pname: String) -> void:
	if _detail_box == null:
		return
	_clear_children(_detail_box)
	_detail_scroll.scroll_vertical = 0
	if pname == "":
		_note(_detail_box, _t("plugins.detail.none", "Select a plugin to see its live registry view."))
		return
	var p := _plugin_by_name(pname)
	var status := str(p.get("status", ""))

	var head := HBoxContainer.new()
	head.add_theme_constant_override("separation", 8)
	_detail_box.add_child(head)
	var name_label := Label.new()
	name_label.add_theme_font_size_override("font_size", DshTokens.FONT_CHROME_LG)
	name_label.add_theme_color_override("font_color", DshTokens.text_primary())
	name_label.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	name_label.text = pname
	head.add_child(name_label)
	_add_chip(head, _status_text(status), _status_color(status))

	_add_section_metadata(p, status)

	# Owned runtime capabilities, attributed by the backend's plugin/owner
	# fields or the "<plugin>__" tool-name prefix.
	var caps := _plugin_caps(pname)
	var tool_count := 0
	var command_count := 0
	for c in caps:
		if str((c as Dictionary).get("kind", "tool")) == "command":
			command_count += 1
		else:
			tool_count += 1
	var cap_content := _add_section(_detail_box, _t("plugins.detail.tools", "Tools & commands"), "%d T · %d C" % [tool_count, command_count])
	if caps.is_empty():
		_note(cap_content, _t("plugins.detail.noneCap", "No tools or commands registered."))
	for c in caps:
		_add_cap_row(cap_content, c, pname)

	# Event topics follow the backend "<plugin>.<event>" naming.
	var evs := _plugin_events(pname)
	var ev_content := _add_section(_detail_box, _t("plugins.detail.events", "Events"), str(evs.size()))
	if evs.is_empty():
		_note(ev_content, _t("plugins.detail.noneEvents", "No event topics."))
	for e in evs:
		_add_event_row(ev_content, e)

	var perms := _plugin_perms(pname, caps)
	var perm_content := _add_section(_detail_box, _t("plugins.detail.permissions", "Permissions"), str(perms.size()))
	if perms.is_empty():
		_note(perm_content, _t("plugins.detail.nonePerms", "No permission gates."))
	for perm in perms:
		_add_perm_row(perm_content, perm)


func _add_section_metadata(p: Dictionary, status: String) -> void:
	var content := _add_section(_detail_box, _t("plugins.detail.metadata", "Metadata"), "")
	_add_kv(content, _t("plugins.detail.source", "Source"), str(p.get("source", "")), DshTokens.text_secondary())
	_add_kv(content, _t("plugins.detail.status", "Status"), _status_text(status), _status_color(status))
	_add_kv(content, "ABI", "v%d" % int(p.get("abiVersion", 0)), DshTokens.text_secondary())
	var command := str(p.get("command", "")).strip_edges()
	if command != "":
		_add_kv(content, _t("plugins.detail.command", "Command"), command, DshTokens.text_secondary())
	var manifest_caps: Variant = p.get("capabilities", [])
	if manifest_caps is Array and not (manifest_caps as Array).is_empty():
		var parts := PackedStringArray()
		for v in (manifest_caps as Array):
			parts.append(str(v))
		_add_kv(content, _t("plugins.detail.manifest", "Manifest caps"), ", ".join(parts), DshTokens.text_secondary())
	if status == "error":
		var err_text := str(p.get("error", "")).strip_edges()
		if err_text != "":
			_add_kv(content, _t("plugins.detail.error", "Error"), err_text, DshTokens.danger())


func _add_section(parent: Control, title: String, sub: String) -> VBoxContainer:
	var panel := PanelContainer.new()
	panel.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	panel.add_theme_stylebox_override("panel", DshTokens.box(
		DshTokens.bg_layer2(),
		DshTokens.RADIUS_MD,
		DshTokens.border_l1(),
		1,
		Vector4(12, 10, 12, 10)
	))
	parent.add_child(panel)
	var v := VBoxContainer.new()
	v.add_theme_constant_override("separation", 6)
	panel.add_child(v)
	var head := HBoxContainer.new()
	head.add_theme_constant_override("separation", 8)
	v.add_child(head)
	var head_label := Label.new()
	head_label.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	head_label.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
	head_label.add_theme_color_override("font_color", DshTokens.text_tertiary())
	head_label.text = title
	head.add_child(head_label)
	if sub != "":
		var sub_label := Label.new()
		sub_label.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
		sub_label.add_theme_color_override("font_color", DshTokens.text_muted())
		sub_label.text = sub
		head.add_child(sub_label)
	return v


func _add_kv(parent: Control, key: String, value: String, value_color: Color) -> void:
	var row := HBoxContainer.new()
	row.add_theme_constant_override("separation", 8)
	parent.add_child(row)
	var k := Label.new()
	k.custom_minimum_size = Vector2(78, 0)
	k.add_theme_font_size_override("font_size", DshTokens.FONT_CHROME)
	k.add_theme_color_override("font_color", DshTokens.text_tertiary())
	k.text = key
	row.add_child(k)
	var val := Label.new()
	val.add_theme_font_size_override("font_size", DshTokens.FONT_CHROME)
	val.add_theme_color_override("font_color", value_color)
	val.autowrap_mode = TextServer.AUTOWRAP_WORD_SMART
	val.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	val.text = value
	row.add_child(val)


func _add_cap_row(parent: Control, c: Dictionary, pname: String) -> void:
	var full := str(c.get("name", ""))
	var kind := str(c.get("kind", "tool"))
	var short_name := full
	if pname != "" and full.begins_with(pname + "__"):
		short_name = full.substr((pname + "__").length())
	var v := VBoxContainer.new()
	v.add_theme_constant_override("separation", 2)
	parent.add_child(v)
	var row := HBoxContainer.new()
	row.add_theme_constant_override("separation", 8)
	v.add_child(row)
	var name_label := Label.new()
	name_label.add_theme_font_size_override("font_size", DshTokens.FONT_CHROME)
	name_label.add_theme_color_override("font_color", DshTokens.text_primary())
	name_label.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	name_label.text = short_name
	row.add_child(name_label)
	_add_chip(row, "command" if kind == "command" else "tool", DshTokens.text_tertiary())
	if bool(c.get("requiresPerm", false)):
		_add_chip(row, "perm", DshTokens.warn())
	var parts := PackedStringArray()
	var desc := str(c.get("description", "")).strip_edges()
	if desc != "":
		parts.append(desc)
	var schema := str(c.get("inputSchema", "")).strip_edges()
	if schema != "":
		parts.append("schema(%d B)" % schema.length())
	if not parts.is_empty():
		var caption := Label.new()
		caption.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
		caption.add_theme_color_override("font_color", DshTokens.text_tertiary())
		caption.autowrap_mode = TextServer.AUTOWRAP_WORD_SMART
		caption.text = " · ".join(parts)
		v.add_child(caption)


func _add_event_row(parent: Control, e: Dictionary) -> void:
	var row := HBoxContainer.new()
	row.add_theme_constant_override("separation", 8)
	parent.add_child(row)
	var topic := Label.new()
	topic.add_theme_font_size_override("font_size", DshTokens.FONT_CHROME)
	topic.add_theme_color_override("font_color", DshTokens.text_secondary())
	topic.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	topic.text = str(e.get("topic", ""))
	row.add_child(topic)
	var count := Label.new()
	count.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
	count.add_theme_color_override("font_color", DshTokens.text_muted())
	count.text = _subscribers_text(int(e.get("subscribers", 0)))
	row.add_child(count)


func _add_perm_row(parent: Control, perm: Dictionary) -> void:
	var row := HBoxContainer.new()
	row.add_theme_constant_override("separation", 8)
	parent.add_child(row)
	var tool := Label.new()
	tool.add_theme_font_size_override("font_size", DshTokens.FONT_CHROME)
	tool.add_theme_color_override("font_color", DshTokens.text_primary())
	tool.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	tool.text = str(perm.get("tool", ""))
	row.add_child(tool)
	var defaults := Label.new()
	defaults.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
	defaults.add_theme_color_override("font_color", DshTokens.text_tertiary())
	defaults.text = "%s: %s · %s: %s" % [
		_t("plugins.detail.approval", "approval"), str(perm.get("defaultApproval", "")),
		_t("plugins.detail.sandbox", "sandbox"), str(perm.get("defaultSandbox", "")),
	]
	row.add_child(defaults)


func _add_chip(parent: Control, text: String, color: Color) -> void:
	var chip := PanelContainer.new()
	var bg := color
	bg.a = 0.14
	chip.size_flags_vertical = Control.SIZE_SHRINK_CENTER
	chip.add_theme_stylebox_override("panel", DshTokens.box(
		bg,
		DshTokens.RADIUS_MD,
		Color.TRANSPARENT,
		0,
		Vector4(8, 2, 8, 2)
	))
	parent.add_child(chip)
	var l := Label.new()
	l.add_theme_font_size_override("font_size", DshTokens.FONT_MICRO)
	l.add_theme_color_override("font_color", color)
	l.text = text
	chip.add_child(l)


func _note(parent: Control, text: String) -> void:
	var l := Label.new()
	l.autowrap_mode = TextServer.AUTOWRAP_WORD_SMART
	l.add_theme_font_size_override("font_size", DshTokens.FONT_CHROME)
	l.add_theme_color_override("font_color", DshTokens.text_tertiary())
	l.text = text
	parent.add_child(l)


func _clear_children(node: Node) -> void:
	for child in node.get_children():
		(child as Node).queue_free()


func _plugin_by_name(pname: String) -> Dictionary:
	for p in _plugins:
		if p is Dictionary and str((p as Dictionary).get("name", (p as Dictionary).get("id", ""))) == pname:
			return p
	return {}


func _plugin_caps(pname: String) -> Array:
	var out: Array = []
	for cap in _capabilities:
		if not (cap is Dictionary):
			continue
		var c := cap as Dictionary
		if _owns(pname, str(c.get("plugin", "")), str(c.get("owner", "")), str(c.get("name", ""))):
			out.append(c)
	return out


func _plugin_events(pname: String) -> Array:
	var out: Array = []
	for ev in _events:
		if not (ev is Dictionary):
			continue
		if str((ev as Dictionary).get("topic", "")).begins_with(pname + "."):
			out.append(ev)
	return out


func _plugin_perms(pname: String, owned: Array) -> Array:
	var names := {}
	for c in owned:
		names[str((c as Dictionary).get("name", ""))] = true
	var out: Array = []
	for perm in _permissions:
		if not (perm is Dictionary):
			continue
		var tool := str((perm as Dictionary).get("tool", ""))
		if names.has(tool) or (pname != "" and tool.begins_with(pname + "__")):
			out.append(perm)
	return out


func _owns(pname: String, via_plugin: String, via_owner: String, tool_name: String) -> bool:
	if pname == "":
		return false
	if via_plugin == pname or via_owner == pname or tool_name.begins_with(pname + "__"):
		return true
	if str(_tool_ownership.get(tool_name, "")) == pname:
		return true
	if str(_command_ownership.get(tool_name, "")) == pname:
		return true
	return false


func _status_color(status: String) -> Color:
	match status:
		"mounted":
			return DshTokens.success()
		"installed":
			return DshTokens.accent()
		"disabled":
			return DshTokens.text_tertiary()
		"error":
			return DshTokens.danger()
	return DshTokens.text_muted()


func _status_text(status: String) -> String:
	if status == "":
		return "—"
	return _t("plugins.status." + status, status)


func _status_mark(status: String) -> String:
	match status:
		"disabled":
			return "○"
		"error":
			return "✕"
	return "●"


func _list_color(status: String) -> Color:
	match status:
		"error":
			return DshTokens.danger()
		"disabled":
			return DshTokens.text_tertiary()
		"installed":
			return DshTokens.text_secondary()
	return DshTokens.text_primary()


func _subscribers_text(n: int) -> String:
	if n == 1:
		return "1 " + _t("plugins.detail.subscriber", "subscriber")
	return "%d %s" % [n, _t("plugins.detail.subscribers", "subscribers")]


func _is_disabled(p: Dictionary) -> bool:
	var status := str(p.get("status", ""))
	if status != "":
		return status == "disabled"
	return not bool(p.get("enabled", p.get("active", true)))


func _current() -> Dictionary:
	var sel := _list.get_selected_items()
	if sel.is_empty():
		return {}
	var meta: Variant = _list.get_item_metadata(sel[0])
	return meta if meta is Dictionary else {}


func _refresh_enable_label() -> void:
	var p := _current()
	if p.is_empty():
		_enable_btn.text = _t("plugins.enable", "Enable")
		_enable_btn.disabled = true
		_uninstall_btn.disabled = true
		return
	_enable_btn.disabled = false
	# Backend PluginInfo carries "source" ("builtin"|"external"), not "enabled".
	_uninstall_btn.disabled = str(p.get("source", "")) == "builtin"
	_enable_btn.text = _t("plugins.enable", "Enable") if _is_disabled(p) else _t("plugins.disable", "Disable")


func _toggle_enabled() -> void:
	var p := _current()
	var pname := str(p.get("name", p.get("id", "")))
	if _client == null or pname == "":
		return
	# 后端以 plugin.enable({name, enabled}) 承载双向开关，无独立 plugin.disable RPC。
	_client.plugin_enable(pname, _is_disabled(p), _on_mutated)


func _uninstall_selected() -> void:
	var p := _current()
	var pname := str(p.get("name", p.get("id", "")))
	if _client == null or pname == "":
		return
	_client.plugin_uninstall(pname, _on_mutated)


func _pick_install() -> void:
	if _dialog == null:
		return
	_dialog.open({
		"mode": "any",
		"title": _t("plugins.install", "Install plugin"),
		"filters": PackedStringArray(["*.zip ; Zip", "*.json ; Manifest"]),
	})


func _install_path(path: String) -> void:
	if _client == null or path.strip_edges() == "":
		return
	_client.plugin_install(path.strip_edges(), _on_mutated)


func _on_mutated(ok: bool, data: Variant) -> void:
	if not ok:
		_set_status(_rpc_err(data, _t("plugins.failed", "Plugin action failed.")), true)
	else:
		_set_status(_t("common.saved", "Saved"), false)
	_refresh()


func _set_status(text: String, failure: bool) -> void:
	_status.text = text
	_status.add_theme_color_override(
		"font_color",
		DshTokens.danger() if failure else DshTokens.text_tertiary()
	)


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