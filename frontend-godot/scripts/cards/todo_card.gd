extends PanelContainer
class_name TodoCard

@onready var header_label: Label = %Header
@onready var list: VBoxContainer = %List
@onready var icon_rect: TextureRect = %Icon

var _data: Dictionary = {}


func _ready() -> void:
	_apply_style()
	icon_rect.texture = load("res://assets/icons/icon_check.svg") as Texture2D
	icon_rect.modulate = DshTokens.success()
	_refresh()


func _notification(what: int) -> void:
	if what == NOTIFICATION_THEME_CHANGED:
		_apply_style()
		_refresh()


func bind(node: Dictionary) -> void:
	setup(node.get("payload", {}) if node.get("payload") is Dictionary else node)


func setup(data: Dictionary) -> void:
	_data = data
	if is_node_ready():
		_refresh()


func _apply_style() -> void:
	add_theme_stylebox_override("panel", DshTokens.box(
		DshTokens.bg_layer2(),
		DshTokens.RADIUS_MD,
		DshTokens.border_l2(),
		1,
		Vector4(12, 8, 12, 8)
	))
	header_label.add_theme_color_override("font_color", DshTokens.text_primary())
	header_label.add_theme_font_size_override("font_size", DshTokens.FONT_CHROME)
	if icon_rect:
		icon_rect.modulate = DshTokens.success()


func _refresh() -> void:
	if list == null:
		return
	for c in list.get_children():
		c.queue_free()
	var todos: Array = _data.get("todos", []) as Array
	var done := 0
	for it in todos:
		if it is Dictionary and _kind(str(it.get("status", ""))) == "done":
			done += 1
	var tmpl := _t("chat.taskChecklist", "Tasks  %d/%d")
	if tmpl.find("%d") >= 0:
		header_label.text = tmpl % [done, todos.size()]
	else:
		header_label.text = "%s  %d/%d" % [tmpl, done, todos.size()]
	if todos.is_empty():
		list.add_child(_item_label(_t("chat.noTodos", "No tasks."), DshTokens.text_muted(), false))
		return
	for it in todos:
		if not (it is Dictionary):
			continue
		var content := str(it.get("content", ""))
		var k := _kind(str(it.get("status", "")))
		match k:
			"done":
				list.add_child(_row("✓", content, DshTokens.success(), DshTokens.text_tertiary(), true))
			"progress":
				list.add_child(_row("●", content, DshTokens.warn(), DshTokens.text_primary(), false, true))
			_:
				list.add_child(_row("○", content, DshTokens.text_muted(), DshTokens.text_secondary(), false))


func _row(mark: String, text: String, mark_color: Color, text_color: Color, strike: bool, bold: bool = false) -> Control:
	var row := HBoxContainer.new()
	row.add_theme_constant_override("separation", 8)
	var m := Label.new()
	m.text = mark
	m.add_theme_color_override("font_color", mark_color)
	m.add_theme_font_size_override("font_size", DshTokens.FONT_UI)
	row.add_child(m)
	var t := Label.new()
	t.text = text
	t.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	t.autowrap_mode = TextServer.AUTOWRAP_WORD_SMART
	t.add_theme_color_override("font_color", text_color)
	t.add_theme_font_size_override("font_size", DshTokens.FONT_CHROME)
	if strike:
		t.add_theme_color_override("font_color", DshTokens.text_tertiary())
	if bold:
		t.add_theme_font_size_override("font_size", DshTokens.FONT_UI)
	row.add_child(t)
	return row


func _item_label(text: String, color: Color, _unused: bool) -> Label:
	var l := Label.new()
	l.text = text
	l.add_theme_color_override("font_color", color)
	l.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
	return l


func _kind(status: String) -> String:
	match status:
		"completed", "done":
			return "done"
		"in_progress", "progress", "running":
			return "progress"
		_:
			return "pending"


func _t(key: String, fallback: String) -> String:
	var s := DshI18n.t(key)
	return fallback if s == key else s
