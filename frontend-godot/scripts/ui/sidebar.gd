extends PanelContainer
class_name SidebarPane

signal new_session_pressed
signal session_selected(id: String)
signal workspace_pick_pressed
signal settings_pressed
signal plugins_pressed
signal theme_toggled
signal collapse_pressed
signal session_rename_requested(id: String, title: String)
signal session_delete_requested(id: String)

@onready var _margin: MarginContainer = %Margin
@onready var _brand_row: HBoxContainer = %BrandRow
@onready var _mark: TextureRect = %BrandMark
@onready var _title: Label = %BrandTitle
@onready var _collapse: Button = %CollapseBtn
@onready var _collapse_icon: TextureRect = %CollapseIcon
@onready var _new_btn: Button = %NewSessionBtn
@onready var _new_icon: TextureRect = %NewSessionIcon
@onready var _new_label: Label = %NewSessionLabel
@onready var _workspace: Button = %WorkspaceBtn
@onready var _ws_icon: TextureRect = %WorkspaceIcon
@onready var _session_label: Label = %SessionLabel
@onready var _search_row: HBoxContainer = %SessionSearchRow
@onready var _search_icon: TextureRect = %SearchIcon
@onready var _search: LineEdit = %SessionSearch
@onready var _list: ItemList = %SessionList
@onready var _status_dot: ColorRect = %StatusDot
@onready var _status_label: Label = %StatusLabel
@onready var _theme_btn: Button = %ThemeBtn
@onready var _plugins_btn: Button = %PluginsBtn
@onready var _plugins_icon: TextureRect = %PluginsIcon
@onready var _settings_btn: Button = %SettingsBtn
@onready var _settings_icon: TextureRect = %SettingsIcon

@onready var _vbox: VBoxContainer = $Margin/VBox
@onready var _footer: HBoxContainer = $Margin/VBox/Footer

var _collapsed := false
var _syncing := false
var _status_ok := true
var _lineage: SubagentTree
var _lineage_label: Label
var _sessions: Array = []
var _active_id := ""
var _ctx_menu: PopupMenu = null
var _ctx_target_id: String = ""
var _rename_editor: LineEdit = null
var _rename_overlay: PanelContainer = null
var _rename_target_id: String = ""
var _delete_dialog: ConfirmationDialog = null
var _delete_target_id: String = ""

func _ready() -> void:
	clip_contents = true
	_collapse.pressed.connect(func(): collapse_pressed.emit())
	_collapse.focus_mode = Control.FOCUS_ALL
	_collapse.mouse_filter = Control.MOUSE_FILTER_STOP
	_collapse.custom_minimum_size = Vector2(28, 28)
	_collapse.size_flags_vertical = Control.SIZE_SHRINK_CENTER
	_brand_row.alignment = BoxContainer.ALIGNMENT_BEGIN
	_collapse.mouse_default_cursor_shape = Control.CURSOR_POINTING_HAND
	_collapse.tooltip_text = _t("toggle.collapse", "Collapse sidebar")
	_new_btn.pressed.connect(func(): new_session_pressed.emit())
	_workspace.pressed.connect(func(): workspace_pick_pressed.emit())
	_theme_btn.pressed.connect(func(): theme_toggled.emit())
	_plugins_btn.pressed.connect(func(): plugins_pressed.emit())
	_settings_btn.pressed.connect(func(): settings_pressed.emit())
	_list.item_selected.connect(_on_item_selected)
	_list.gui_input.connect(_on_list_gui_input)
	_list.item_clicked.connect(_on_list_item_clicked)
	_search.text_changed.connect(func(_t: String) -> void: _rebuild_session_list())
	if DshI18n.has_signal("locale_changed"):
		DshI18n.locale_changed.connect(func(_loc: String): _apply_strings())
	_lineage_label = Label.new()
	_lineage_label.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_vbox.add_child(_lineage_label)
	_lineage = SubagentTree.new()
	_lineage.custom_minimum_size = Vector2(0, 110)
	_lineage.size_flags_vertical = Control.SIZE_EXPAND_FILL
	_lineage.subagent_selected.connect(func(id: String) -> void: session_selected.emit(id))
	_vbox.add_child(_lineage)
	_vbox.move_child(_lineage_label, _footer.get_index())
	_vbox.move_child(_lineage, _footer.get_index())
	apply_tokens()
	_apply_strings()
	set_status(_t("app.ready", "Ready"), true)
	_build_context_menu()
	_build_rename_overlay()
	_build_delete_dialog()


func apply_tokens() -> void:
	var sb := DshTokens.box(DshTokens.bg_sidebar(), 0, DshTokens.border_l1(), 1, Vector4.ZERO)
	sb.border_width_left = 0
	sb.border_width_top = 0
	sb.border_width_bottom = 0
	sb.shadow_color = DshTokens.shadow_tinted()
	sb.shadow_size = 8
	add_theme_stylebox_override("panel", sb)
	DshIcons.apply_brand(_mark, 24.0)
	DshIcons.apply(_collapse_icon, "panel_left", 16.0)
	DshIcons.apply(_new_icon, "new_chat", 14.0, true)
	DshIcons.apply(_ws_icon, "folder", 16.0)
	_ws_icon.visible = false
	_workspace.icon = DshIcons.texture("folder")
	_workspace.add_theme_color_override("icon_normal_color", DshTokens.text_secondary())
	DshIcons.apply(_search_icon, "search", 14.0)
	_search.add_theme_color_override("font_color", DshTokens.text_primary())
	_search.add_theme_color_override("font_placeholder_color", DshTokens.text_tertiary())
	_search.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
	_search.add_theme_stylebox_override("normal", DshTokens.box(DshTokens.bg_layer2(), DshTokens.RADIUS_PILL, DshTokens.border_l1(), 1, Vector4(10, 4, 10, 4)))
	_search.add_theme_stylebox_override("focus", DshTokens.box(DshTokens.bg_layer2(), DshTokens.RADIUS_PILL, DshTokens.accent(), 1, Vector4(10, 4, 10, 4)))
	DshIcons.apply(_plugins_icon, "puzzle", 16.0)
	if _plugins_icon.texture == null:
		DshIcons.apply(_plugins_icon, "plan", 16.0)
	DshIcons.apply(_settings_icon, "settings", 16.0)
	_title.add_theme_color_override("font_color", DshTokens.text_primary())
	_title.add_theme_font_size_override("font_size", 15)
	_session_label.add_theme_color_override("font_color", DshTokens.text_tertiary())
	_session_label.add_theme_font_size_override("font_size", DshTokens.FONT_MICRO)
	_status_label.add_theme_color_override("font_color", DshTokens.text_secondary())
	_status_label.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
	_paint_new_btn()
	_paint_icon_btn(_collapse)
	_paint_icon_btn(_plugins_btn)
	_paint_icon_btn(_settings_btn)
	_paint_icon_btn(_theme_btn)
	_workspace.add_theme_stylebox_override("normal", DshTokens.box(DshTokens.bg_layer2(), DshTokens.RADIUS_SM, DshTokens.border_l1(), 1, Vector4(8, 6, 8, 6)))
	_workspace.add_theme_stylebox_override("hover", DshTokens.box(DshTokens.bg_layer3(), DshTokens.RADIUS_SM, DshTokens.border_l2(), 1, Vector4(8, 6, 8, 6)))
	_workspace.add_theme_color_override("font_color", DshTokens.text_primary())
	_status_dot.color = DshTokens.success() if _status_ok else DshTokens.danger()
	_apply_strings()


func set_sessions(arr: Array, active_id: String) -> void:
	_sessions = arr
	_active_id = active_id
	_rebuild_session_list()
	if _lineage != null:
		_lineage.ingest_sessions(arr)


func set_collapsed(collapsed: bool) -> void:
	if _collapsed == collapsed:
		return
	_collapsed = collapsed
	var pad := 14 if collapsed else 12
	_margin.add_theme_constant_override("margin_left", pad)
	_margin.add_theme_constant_override("margin_right", pad)
	_title.visible = not collapsed
	_mark.visible = not collapsed
	_brand_row.alignment = BoxContainer.ALIGNMENT_CENTER if collapsed else BoxContainer.ALIGNMENT_BEGIN
	_workspace.visible = not collapsed
	_session_label.visible = not collapsed
	_search_row.visible = not collapsed
	_list.visible = not collapsed
	if _lineage_label != null:
		_lineage_label.visible = not collapsed
	if _lineage != null:
		_lineage.visible = not collapsed
	_status_label.visible = not collapsed
	_status_dot.visible = not collapsed
	_plugins_btn.visible = not collapsed
	_settings_btn.visible = not collapsed
	_theme_btn.visible = not collapsed
	_footer.visible = not collapsed
	_collapse.visible = true
	if _collapse_icon != null:
		DshIcons.apply(_collapse_icon, "panel_left" if not collapsed else "panel_left", 16.0)
		_collapse_icon.rotation_degrees = 180.0 if collapsed else 0.0
		_collapse_icon.mouse_filter = Control.MOUSE_FILTER_IGNORE
		_collapse.mouse_filter = Control.MOUSE_FILTER_STOP
		_collapse.focus_mode = Control.FOCUS_ALL
	_collapse.tooltip_text = _t("toggle.open", "Open sidebar") if collapsed else _t("toggle.collapse", "Collapse sidebar")
	_new_label.visible = not collapsed
	if _new_btn != null:
		_new_btn.tooltip_text = _t("toggle.open", "Open sidebar") if collapsed else _t("app.newSession", "New Session")
	_apply_strings()
	if collapsed:
		_collapse.tooltip_text = _t("toggle.open", "Open sidebar")
		if _collapse_icon != null:
			_collapse_icon.rotation_degrees = 180.0


func set_status(text: String, ok: bool) -> void:
	_status_ok = ok
	_status_label.text = text
	_status_dot.color = DshTokens.success() if ok else DshTokens.danger()


func handle_host_event(method: String, payload: Variant) -> void:
	if _lineage != null:
		_lineage.handle_host_event(method, payload)


func set_workspace_label(text: String) -> void:
	_workspace.text = text if text != "" else _t("hero.chooseWorkspace", "Choose workspace")
	_workspace.tooltip_text = text


func _rebuild_session_list() -> void:
	var q := ""
	if _search != null:
		q = _search.text.strip_edges().to_lower()
	_syncing = true
	_list.clear()
	var select := -1
	for s in _sessions:
		if not (s is Dictionary):
			continue
		var id := str(s.get("id", ""))
		if id == "":
			continue
		if not _session_matches(s, q):
			continue
		_list.add_item(_session_title(s))
		var idx := _list.item_count - 1
		_list.set_item_metadata(idx, id)
		if id == _active_id:
			select = idx
	if select >= 0:
		_list.select(select)
	_syncing = false


func _session_matches(s: Dictionary, q: String) -> bool:
	if q == "":
		return true
	if _session_title(s).to_lower().find(q) >= 0:
		return true
	for key in ["title", "id", "cwd"]:
		if str(s.get(key, "")).to_lower().find(q) >= 0:
			return true
	return false


func _on_item_selected(index: int) -> void:
	if _syncing:
		return
	var meta: Variant = _list.get_item_metadata(index)
	if str(meta) != "":
		session_selected.emit(str(meta))


func _session_title(s: Dictionary) -> String:
	var title := str(s.get("title", ""))
	if title != "":
		return title
	var cwd := str(s.get("cwd", ""))
	if cwd != "":
		var base := cwd.get_file()
		return base if base != "" else cwd
	var id := str(s.get("id", ""))
	return id.substr(0, 8) if id.length() > 8 else id


func _apply_strings() -> void:
	_title.text = "DSHX"
	_new_label.text = _t("app.newSession", "New Session")
	_session_label.text = _t("app.recentSessions", "Recent sessions").to_upper()
	_search.placeholder_text = _t("app.searchSessions", "Search sessions…")
	_search.tooltip_text = "%s (Ctrl+F)" % _t("app.searchSessions", "Search sessions…")
	if _lineage_label != null:
		_lineage_label.text = _t("app.agentTeams", "Agent teams").to_upper()
		_lineage_label.add_theme_color_override("font_color", DshTokens.text_tertiary())
		_lineage_label.add_theme_font_size_override("font_size", DshTokens.FONT_MICRO)
	_theme_btn.text = _t("app.themeLight", "Light") if DshTokens.is_dark() else _t("app.themeDark", "Dark")
	if not _collapsed:
		_collapse.tooltip_text = _t("toggle.collapse", "Collapse sidebar")
		_new_btn.tooltip_text = "%s (Ctrl+N)" % _t("app.newSession", "New Session")
	_settings_btn.tooltip_text = "%s (Ctrl+,)" % _t("common.settings", "Settings")
	_plugins_btn.tooltip_text = _t("app.plugins", "Plugins")
	_theme_btn.tooltip_text = _t("app.themeDark", "Theme")
	if _workspace.text == "" or _workspace.text == "Workspace":
		_workspace.text = _t("hero.chooseWorkspace", "Choose workspace")
	# Keep context menu labels in sync with locale.
	if _ctx_menu != null:
		_ctx_menu.set_item_text(_ctx_menu.get_item_index(0), _t("common.rename", "Rename"))
		_ctx_menu.set_item_text(_ctx_menu.get_item_index(1), _t("common.delete", "Delete"))
		_ctx_menu.set_item_text(_ctx_menu.get_item_index(2), _t("session.copyId", "Copy session ID"))
	if _rename_editor != null:
		_rename_editor.placeholder_text = _t("session.renameTitle", "Rename session")
	if _delete_dialog != null:
		_delete_dialog.dialog_text = _t("session.deleteConfirm", "Delete this session? This cannot be undone.")
		_delete_dialog.ok_button_text = _t("common.delete", "Delete")
		_delete_dialog.title = _t("session.renameTitle", "Rename session") if _delete_target_id == "" else _t("common.delete", "Delete")


func _paint_new_btn() -> void:
	var bg := DshTokens.brand_button()
	var fg := DshTokens.bg_base()
	var pad := Vector4(10, 8, 10, 8)
	_new_btn.add_theme_stylebox_override("normal", DshTokens.box(bg, DshTokens.RADIUS_MD, Color(0, 0, 0, 0), 0, pad))
	_new_btn.add_theme_stylebox_override("hover", DshTokens.box(DshTokens.text_secondary(), DshTokens.RADIUS_MD, Color(0, 0, 0, 0), 0, pad))
	_new_btn.add_theme_stylebox_override("pressed", DshTokens.box(DshTokens.text_secondary(), DshTokens.RADIUS_MD, Color(0, 0, 0, 0), 0, pad))
	_new_label.add_theme_color_override("font_color", fg)
	_new_icon.modulate = fg


func _paint_icon_btn(btn: Button) -> void:
	var empty := DshTokens.box(Color(0, 0, 0, 0), DshTokens.RADIUS_MD, Color(0, 0, 0, 0), 0, Vector4(4, 4, 4, 4))
	var hover := DshTokens.box(DshTokens.bg_layer2(), DshTokens.RADIUS_MD, Color(0, 0, 0, 0), 0, Vector4(4, 4, 4, 4))
	btn.add_theme_stylebox_override("normal", empty)
	btn.add_theme_stylebox_override("hover", hover)
	btn.add_theme_stylebox_override("pressed", hover)
	btn.add_theme_stylebox_override("focus", empty)
	btn.flat = true


func _build_context_menu() -> void:
	if _ctx_menu != null:
		return
	_ctx_menu = PopupMenu.new()
	_ctx_menu.add_item(_t("common.rename", "Rename"), 0)
	_ctx_menu.add_item(_t("common.delete", "Delete"), 1)
	_ctx_menu.add_item(_t("session.copyId", "Copy session ID"), 2)
	_ctx_menu.id_pressed.connect(_on_context_action)
	add_child(_ctx_menu)


func _build_rename_overlay() -> void:
	if _rename_overlay != null:
		return
	_rename_overlay = PanelContainer.new()
	_rename_overlay.visible = false
	_rename_overlay.add_theme_stylebox_override("panel", DshTokens.box(DshTokens.bg_layer1(), DshTokens.RADIUS_MD, DshTokens.border_l1(), 1, Vector4(6, 6, 6, 6)))
	var row := HBoxContainer.new()
	row.add_theme_constant_override("separation", 6)
	_rename_overlay.add_child(row)
	_rename_editor = LineEdit.new()
	_rename_editor.placeholder_text = _t("session.renameTitle", "Rename session")
	_rename_editor.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_rename_editor.text_submitted.connect(_on_rename_committed)
	# Esc cancels inline rename.
	_rename_editor.gui_input.connect(func(event: InputEvent) -> void:
		var k := event as InputEventKey
		if k != null and k.pressed and not k.echo and k.keycode == KEY_ESCAPE:
			_hide_rename()
			get_viewport().set_input_as_handled()
	)
	row.add_child(_rename_editor)
	var ok := Button.new()
	ok.text = _t("common.ok", "OK")
	ok.pressed.connect(_on_rename_confirmed)
	row.add_child(ok)
	var cancel := Button.new()
	cancel.text = _t("common.cancel", "Cancel")
	cancel.pressed.connect(_hide_rename)
	row.add_child(cancel)
	add_child(_rename_overlay)


func _build_delete_dialog() -> void:
	if _delete_dialog != null:
		return
	_delete_dialog = ConfirmationDialog.new()
	_delete_dialog.title = _t("common.delete", "Delete")
	_delete_dialog.dialog_text = _t("session.deleteConfirm", "Delete this session? This cannot be undone.")
	_delete_dialog.ok_button_text = _t("common.delete", "Delete")
	_delete_dialog.confirmed.connect(_on_delete_confirmed)
	add_child(_delete_dialog)


func _on_list_gui_input(event: InputEvent) -> void:
	if event is InputEventMouseButton:
		var mb := event as InputEventMouseButton
		if mb.pressed and mb.button_index == MOUSE_BUTTON_RIGHT:
			var pos := _list.get_local_mouse_position()
			var idx := _list.get_item_at_position(pos, true)
			if idx >= 0 and idx < _list.item_count:
				_ctx_target_id = str(_list.get_item_metadata(idx))
				_list.select(idx)
				# Ensure single selection semantics for context actions.
				if not _syncing and _ctx_target_id != "":
					# Do not navigate; just set target for menu.
					pass
				_show_context_menu(mb.global_position)
				get_viewport().set_input_as_handled()


func _on_list_item_clicked(index: int, at_pos: Vector2, button_index: int) -> void:
	if button_index == MOUSE_BUTTON_RIGHT and index >= 0 and index < _list.item_count:
		_ctx_target_id = str(_list.get_item_metadata(index))
		_show_context_menu(get_viewport().get_mouse_position())


func _show_context_menu(global_pos: Vector2) -> void:
	if _ctx_menu == null:
		return
	_ctx_menu.position = Vector2i(global_pos)
	_ctx_menu.popup()


func _on_context_action(id: int) -> void:
	var target := _ctx_target_id
	if target == "":
		return
	match id:
		0:
			_begin_rename(target)
		1:
			_begin_delete(target)
		2:
			_copy_session_id(target)


func _begin_rename(id: String) -> void:
	_rename_target_id = id
	var cur := ""
	for s in _sessions:
		if s is Dictionary and str(s.get("id", "")) == id:
			cur = _session_title(s as Dictionary)
			break
	if _rename_editor != null:
		_rename_editor.text = cur
		_rename_editor.select_all()
	if _rename_overlay != null:
		_rename_overlay.visible = true
		_rename_overlay.global_position = _list.global_position + Vector2(0, _list.size.y + 4)
		_rename_overlay.size = Vector2(_list.size.x, 0)
	if _rename_editor != null:
		_rename_editor.grab_focus()


func _on_rename_committed(text: String) -> void:
	_commit_rename(text)


func _on_rename_confirmed() -> void:
	if _rename_editor != null:
		_commit_rename(_rename_editor.text)


func _commit_rename(title: String) -> void:
	var tid := _rename_target_id
	var t := title.strip_edges()
	if tid == "" or t == "":
		_hide_rename()
		return
	_hide_rename()
	session_rename_requested.emit(tid, t)


func _hide_rename() -> void:
	_rename_target_id = ""
	if _rename_overlay != null:
		_rename_overlay.visible = false


func _begin_delete(id: String) -> void:
	_delete_target_id = id
	if _delete_dialog != null:
		_delete_dialog.popup_centered()


func _on_delete_confirmed() -> void:
	var tid := _delete_target_id
	_delete_target_id = ""
	if tid != "":
		session_delete_requested.emit(tid)


func _copy_session_id(id: String) -> void:
	if id != "":
		DisplayServer.clipboard_set(id)


func focus_search() -> void:
	if _search != null:
		_search.grab_focus()
		_search.select_all()


func _t(key: String, fallback: String) -> String:
	var v := str(DshI18n.t(key))
	return fallback if v == key or v.strip_edges() == "" else v
