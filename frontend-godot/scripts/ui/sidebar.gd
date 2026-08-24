extends PanelContainer
class_name SidebarPane

signal new_session_pressed
signal session_selected(id: String)
signal workspace_pick_pressed
signal settings_pressed
signal plugins_pressed
signal theme_toggled
signal collapse_pressed

@onready var _margin: MarginContainer = %Margin
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

func _ready() -> void:
	clip_contents = true
	_collapse.pressed.connect(func(): collapse_pressed.emit())
	_new_btn.pressed.connect(func(): new_session_pressed.emit())
	_workspace.pressed.connect(func(): workspace_pick_pressed.emit())
	_theme_btn.pressed.connect(func(): theme_toggled.emit())
	_plugins_btn.pressed.connect(func(): plugins_pressed.emit())
	_settings_btn.pressed.connect(func(): settings_pressed.emit())
	_list.item_selected.connect(_on_item_selected)
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


func apply_tokens() -> void:
	var sb := DshTokens.box(DshTokens.bg_sidebar(), 0, DshTokens.border_l1(), 1, Vector4.ZERO)
	sb.border_width_left = 0
	sb.border_width_top = 0
	sb.border_width_bottom = 0
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
	_search.add_theme_stylebox_override("normal", DshTokens.box(DshTokens.bg_layer2(), DshTokens.RADIUS_SM, DshTokens.border_l1(), 1, Vector4(8, 4, 8, 4)))
	_search.add_theme_stylebox_override("focus", DshTokens.box(DshTokens.bg_layer2(), DshTokens.RADIUS_SM, DshTokens.accent(), 1, Vector4(8, 4, 8, 4)))
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
	_collapsed = collapsed
	var pad := 8 if collapsed else 12
	_margin.add_theme_constant_override("margin_left", pad)
	_margin.add_theme_constant_override("margin_right", pad)
	_title.visible = not collapsed
	_workspace.visible = not collapsed
	_session_label.visible = not collapsed
	_search_row.visible = not collapsed
	_list.visible = not collapsed
	if _lineage_label != null:
		_lineage_label.visible = not collapsed
	if _lineage != null:
		_lineage.visible = not collapsed
	_status_label.visible = not collapsed
	_theme_btn.visible = not collapsed
	_collapse.visible = not collapsed
	_new_label.visible = not collapsed
	_apply_strings()


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
	_search.tooltip_text = _t("app.searchSessions", "Search sessions…")
	if _lineage_label != null:
		_lineage_label.text = _t("app.agentTeams", "Agent teams").to_upper()
		_lineage_label.add_theme_color_override("font_color", DshTokens.text_tertiary())
		_lineage_label.add_theme_font_size_override("font_size", DshTokens.FONT_MICRO)
	_theme_btn.text = _t("app.themeLight", "Light") if DshTokens.is_dark() else _t("app.themeDark", "Dark")
	_collapse.tooltip_text = _t("toggle.collapse", "Collapse sidebar") if not _collapsed else _t("toggle.open", "Open sidebar")
	_new_btn.tooltip_text = _t("app.newSession", "New Session")
	_settings_btn.tooltip_text = _t("common.settings", "Settings")
	_plugins_btn.tooltip_text = _t("app.plugins", "Plugins")
	_theme_btn.tooltip_text = _t("app.themeDark", "Theme")
	if _workspace.text == "" or _workspace.text == "Workspace":
		_workspace.text = _t("hero.chooseWorkspace", "Choose workspace")


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


func _t(key: String, fallback: String) -> String:
	var v := str(DshI18n.t(key))
	return fallback if v == key or v.strip_edges() == "" else v
