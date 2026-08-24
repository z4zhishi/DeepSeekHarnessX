extends CenterContainer
class_name HeroView

signal suggestion_clicked(prompt: String)

@onready var _mark: TextureRect = %Mark
@onready var _title: Label = %Title
@onready var _subtitle: Label = %Subtitle
@onready var _grid: GridContainer = %Grid

const _CARDS := [
	{"icon": "terminal", "key": "chat.suggestExplore", "title": "Explore workspace", "desc_key": "chat.suggestExploreDesc", "desc": "Index the tree and explain entry points", "prompt": "Please explore and explain the architecture of this workspace."},
	{"icon": "plan", "key": "chat.suggestPlan", "title": "Draft a plan", "desc_key": "chat.suggestPlanDesc", "desc": "Outline steps before making edits", "prompt": "/plan on"},
	{"icon": "diff", "key": "chat.suggestDiff", "title": "Review git diff", "desc_key": "chat.suggestDiffDesc", "desc": "Inspect local changes and regressions", "prompt": "Check current git diff and review recent changes."},
	{"icon": "check", "key": "chat.suggestTest", "title": "Run tests", "desc_key": "chat.suggestTestDesc", "desc": "Execute the suite and explain failures", "prompt": "Run the project test suite and report results."},
]

func _ready() -> void:
	size_flags_horizontal = Control.SIZE_EXPAND_FILL
	size_flags_vertical = Control.SIZE_EXPAND_FILL
	if DshI18n.has_signal("locale_changed"):
		DshI18n.locale_changed.connect(func(_loc: String): apply_tokens())
	_build_cards()
	apply_tokens()


func apply_tokens() -> void:
	DshIcons.apply_brand(_mark, 34.0)
	_title.add_theme_color_override("font_color", DshTokens.text_primary())
	_title.add_theme_font_size_override("font_size", 26)
	_subtitle.add_theme_color_override("font_color", DshTokens.text_tertiary())
	_subtitle.add_theme_font_size_override("font_size", DshTokens.FONT_UI)
	_title.text = _t("chat.heroTitle", "DSHX")
	_subtitle.text = _t("chat.heroSubtitle", "高性能 Agent 工作台")
	for i in _grid.get_child_count():
		var card := _grid.get_child(i) as Button
		if card == null:
			continue
		_paint_card(card)
		if i < _CARDS.size():
			_label_card(card, _CARDS[i])


func _build_cards() -> void:
	for child in _grid.get_children():
		child.queue_free()
	for spec in _CARDS:
		var card := Button.new()
		card.custom_minimum_size = Vector2(240, 72)
		card.size_flags_horizontal = Control.SIZE_EXPAND_FILL
		card.alignment = HORIZONTAL_ALIGNMENT_LEFT
		card.clip_text = false
		var body := VBoxContainer.new()
		body.mouse_filter = Control.MOUSE_FILTER_IGNORE
		body.add_theme_constant_override("separation", 2)
		card.add_child(body)
		var head := HBoxContainer.new()
		head.mouse_filter = Control.MOUSE_FILTER_IGNORE
		head.add_theme_constant_override("separation", 8)
		body.add_child(head)
		var icon := TextureRect.new()
		icon.name = "Icon"
		head.add_child(icon)
		DshIcons.apply(icon, str(spec["icon"]), 16.0)
		var name_lbl := Label.new()
		name_lbl.name = "Name"
		head.add_child(name_lbl)
		var desc := Label.new()
		desc.name = "Desc"
		desc.autowrap_mode = TextServer.AUTOWRAP_WORD_SMART
		body.add_child(desc)
		var prompt := str(spec["prompt"])
		card.pressed.connect(func(): suggestion_clicked.emit(prompt))
		_grid.add_child(card)
		_paint_card(card)
		_label_card(card, spec)


func _paint_card(card: Button) -> void:
	var pad := Vector4(14, 10, 14, 10)
	card.add_theme_stylebox_override("normal", DshTokens.box(DshTokens.bg_layer2(), DshTokens.RADIUS_MD, DshTokens.border_l1(), 1, pad))
	card.add_theme_stylebox_override("hover", DshTokens.box(DshTokens.bg_layer3(), DshTokens.RADIUS_MD, DshTokens.border_l2(), 1, pad))
	card.add_theme_stylebox_override("pressed", DshTokens.box(DshTokens.bg_layer3(), DshTokens.RADIUS_MD, DshTokens.border_l3(), 1, pad))
	var icon := card.find_child("Icon", true, false) as TextureRect
	if icon:
		DshIcons.paint(icon)
	var name_lbl := card.find_child("Name", true, false) as Label
	if name_lbl:
		name_lbl.add_theme_color_override("font_color", DshTokens.text_primary())
		name_lbl.add_theme_font_size_override("font_size", DshTokens.FONT_CHROME)
	var desc := card.find_child("Desc", true, false) as Label
	if desc:
		desc.add_theme_color_override("font_color", DshTokens.text_tertiary())
		desc.add_theme_font_size_override("font_size", DshTokens.FONT_MICRO)


func _label_card(card: Button, spec: Dictionary) -> void:
	var name_lbl := card.find_child("Name", true, false) as Label
	if name_lbl:
		name_lbl.text = _t(str(spec["key"]), str(spec["title"]))
	var desc := card.find_child("Desc", true, false) as Label
	if desc:
		desc.text = _t(str(spec["desc_key"]), str(spec["desc"]))
	var icon := card.find_child("Icon", true, false) as TextureRect
	if icon:
		DshIcons.apply(icon, str(spec["icon"]), 16.0)


func _t(key: String, fallback: String) -> String:
	var v := str(DshI18n.t(key))
	return fallback if v == key or v.strip_edges() == "" else v
