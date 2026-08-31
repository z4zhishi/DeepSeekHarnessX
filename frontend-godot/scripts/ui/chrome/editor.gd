extends CanvasLayer
class_name DshChromeEditor

## Chrome layout editor overlay. Built entirely in code so
## `DshChromeEditor.new()` works without a scene. Parent instances this.
##
## The card body is a real Phase 3 engine page: documents/chrome_editor_doc.gd
## builds the AST, DshUIReconciler mounts it into `_host`. A hand-built VBox
## fallback exists only if engine mount fails (Godot 4.7 class cache); the
## probe must pass on the engine path (`_use_engine` stays true).

signal layout_saved(data: Dictionary)
signal closed

const DshChromeDocT := preload("res://documents/chrome_editor_doc.gd")
const DshReconT := preload("res://engine/reconciler.gd")
const DshLayoutT := preload("res://scripts/ui/chrome/layout.gd")

const CARD_SIZE := Vector2(560, 500)
const SHORT_TO_SLOT := {
	"left": "composer.left",
	"right": "composer.right",
	"overflow": "composer.overflow",
}

var _use_engine := true
var _opened := false
var _layout = null
var _working: Dictionary = {}
var _recon = null
var _last_stats: Dictionary = {}

var _backdrop: ColorRect
var _stage: Control
var _card: PanelContainer
var _host: MarginContainer


func _ready() -> void:
	layer = 22
	process_mode = Node.PROCESS_MODE_ALWAYS
	_build()
	if not _opened:
		visible = false
	if get_node_or_null("/root/DshI18n") != null and DshI18n.has_signal("locale_changed"):
		DshI18n.locale_changed.connect(func(_loc: String) -> void:
			if _opened:
				_render()
		)


func open() -> void:
	_ensure_layout()
	_layout.load_layout()
	_working = _copy_slots(_layout.as_dict())
	_opened = true
	_build()
	visible = true
	_render()
	_apply_style()
	if is_inside_tree():
		call_deferred("_play_open_motion")
	else:
		_play_open_motion()


func close() -> void:
	var was := _opened
	_opened = false
	visible = false
	if _card != null:
		_card.modulate.a = 1.0
	if _backdrop != null:
		_backdrop.modulate.a = 1.0
	if was:
		closed.emit()


func is_open() -> bool:
	return _opened and visible


func current_layout() -> Dictionary:
	return _copy_slots(_working)


func _build() -> void:
	if _card != null:
		return
	layer = 22

	_backdrop = ColorRect.new()
	_backdrop.color = Color(0, 0, 0, 0.45)
	_backdrop.set_anchors_preset(Control.PRESET_FULL_RECT)
	_backdrop.mouse_filter = Control.MOUSE_FILTER_STOP
	_backdrop.gui_input.connect(_on_backdrop)
	add_child(_backdrop)

	_stage = Control.new()
	_stage.set_anchors_preset(Control.PRESET_FULL_RECT)
	_stage.mouse_filter = Control.MOUSE_FILTER_IGNORE
	add_child(_stage)

	_card = PanelContainer.new()
	_card.custom_minimum_size = CARD_SIZE
	_card.mouse_filter = Control.MOUSE_FILTER_STOP
	_place_card()
	_stage.add_child(_card)

	_host = MarginContainer.new()
	_host.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_host.size_flags_vertical = Control.SIZE_EXPAND_FILL
	_card.add_child(_host)

	_ensure_recon()
	_apply_style()


func _place_card() -> void:
	if _card == null:
		return
	var sz := CARD_SIZE
	_card.anchor_left = 0.5
	_card.anchor_top = 0.5
	_card.anchor_right = 0.5
	_card.anchor_bottom = 0.5
	_card.grow_horizontal = Control.GROW_DIRECTION_BOTH
	_card.grow_vertical = Control.GROW_DIRECTION_BOTH
	_card.offset_left = -sz.x * 0.5
	_card.offset_top = -sz.y * 0.5
	_card.offset_right = sz.x * 0.5
	_card.offset_bottom = sz.y * 0.5


func _play_open_motion() -> void:
	if not visible:
		return
	if _backdrop != null:
		var bc := _backdrop.modulate
		bc.a = 1.0
		_backdrop.modulate = bc
		DshTokens.fade_in(_backdrop, DshTokens.MOTION_BASE)
	if _card != null:
		var cc := _card.modulate
		cc.a = 1.0
		_card.modulate = cc
		DshTokens.slide_in_y(_card, 12.0, DshTokens.MOTION_SNAP)


func _ensure_layout() -> void:
	if _layout == null:
		_layout = DshLayoutT.new()


func _ensure_recon() -> void:
	if _recon != null:
		return
	_recon = DshReconT.new()
	_recon.action.connect(_on_action)


func _render() -> void:
	if _host == null:
		return
	if _use_engine and _try_engine():
		return
	if _use_engine:
		_use_engine = false
	_mount_fallback()


func _try_engine() -> bool:
	_ensure_recon()
	if _recon == null or _host == null:
		return false
	var doc: Dictionary = DshChromeDocT.build(_state_dict())
	if doc.is_empty() or not DshReconT.validate(doc).is_empty():
		return false
	_last_stats = _recon.update(_host, doc)
	var root: Control = null
	if _host.has_meta(DshReconT.META_ROOT):
		root = _host.get_meta(DshReconT.META_ROOT) as Control
	if root == null or not is_instance_valid(root):
		return false
	_paint_tree(root)
	return true


func _state_dict() -> Dictionary:
	return {
		"slots": _copy_slots(_working),
		"palette": _unused_ids(),
	}


func _on_action(action_name: String) -> void:
	if action_name == "chrome.save":
		_save()
		return
	if action_name == "chrome.reset":
		_reset()
		return
	if action_name == "chrome.close":
		close()
		return
	if action_name.begins_with("chrome.move."):
		var parts := action_name.trim_prefix("chrome.move.").split(".")
		if parts.size() >= 3:
			_move_widget(parts[0], parts[1], parts[2])
		call_deferred("_render")
		return
	if action_name.begins_with("chrome.add."):
		var parts := action_name.trim_prefix("chrome.add.").split(".")
		if parts.size() >= 2:
			_add_widget(parts[0], parts[1])
		call_deferred("_render")


func _save() -> void:
	_ensure_layout()
	_apply_working()
	_layout.save_layout()
	layout_saved.emit(_layout.as_dict())


func _reset() -> void:
	_working = DshLayoutT.DEFAULT.duplicate(true)
	_ensure_layout()
	_apply_working()
	_layout.save_layout()
	layout_saved.emit(_layout.as_dict())
	call_deferred("_render")


func _apply_working() -> void:
	# layout._merge skips empty slot arrays, so assign the working copy
	# directly and let save_layout() persist it as-is.
	_layout._slots = _copy_slots(_working)


func _move_widget(short_slot: String, widget_id: String, dir: String) -> void:
	var from_slot := _full_slot(short_slot)
	if from_slot == "" or widget_id.strip_edges() == "":
		return
	var ids := _ids_of(from_slot)
	var idx := ids.find(widget_id)
	if idx < 0:
		return
	if dir == "left":
		if idx > 0:
			var tmp: Variant = ids[idx - 1]
			ids[idx - 1] = ids[idx]
			ids[idx] = tmp
			_working[from_slot] = ids
		return
	if dir == "right":
		if idx < ids.size() - 1:
			var tmp: Variant = ids[idx + 1]
			ids[idx + 1] = ids[idx]
			ids[idx] = tmp
			_working[from_slot] = ids
		return
	if dir == "out":
		ids.remove_at(idx)
		_working[from_slot] = ids
		return
	var target_short := dir
	if dir.begins_with("to_"):
		target_short = dir.trim_prefix("to_")
	var to_slot := _full_slot(target_short)
	if to_slot == "" or to_slot == from_slot:
		return
	ids.remove_at(idx)
	_working[from_slot] = ids
	var dest := _ids_of(to_slot)
	if dest.find(widget_id) < 0:
		dest.append(widget_id)
	_working[to_slot] = dest


func _add_widget(short_slot: String, widget_id: String) -> void:
	widget_id = widget_id.strip_edges()
	if widget_id == "" or not DshLayoutT.KNOWN.has(widget_id):
		return
	if _is_used(widget_id):
		return
	var to_slot := _full_slot(short_slot)
	if to_slot == "":
		return
	var dest := _ids_of(to_slot)
	dest.append(widget_id)
	_working[to_slot] = dest


func _full_slot(short_slot: String) -> String:
	return str(SHORT_TO_SLOT.get(short_slot, ""))


func _ids_of(slot_id: String) -> Array:
	var out: Array = []
	var raw: Variant = _working.get(slot_id, [])
	if not (raw is Array):
		return out
	for item in raw:
		var id := str(item).strip_edges()
		if id != "":
			out.append(id)
	return out


func _unused_ids() -> Array:
	var used := {}
	for slot_id in SHORT_TO_SLOT.values():
		for id in _ids_of(str(slot_id)):
			used[id] = true
	var out: Array = []
	for id in DshLayoutT.KNOWN:
		if not used.has(id):
			out.append(id)
	return out


func _is_used(widget_id: String) -> bool:
	for slot_id in SHORT_TO_SLOT.values():
		if _ids_of(str(slot_id)).find(widget_id) >= 0:
			return true
	return false


func _copy_slots(src: Dictionary) -> Dictionary:
	var out := {}
	for key in SHORT_TO_SLOT.values():
		var slot := str(key)
		var ids: Array = []
		var raw: Variant = src.get(slot, [])
		if raw is Array:
			for item in raw:
				var id := str(item).strip_edges()
				if id != "" and DshLayoutT.KNOWN.has(id) and not ids.has(id):
					ids.append(id)
		out[slot] = ids
	return out


func _mount_fallback() -> void:
	if _recon != null and _host != null:
		_recon.unmount(_host)
	if _host == null:
		return
	for child in _host.get_children():
		_host.remove_child(child)
		child.free()
	var box := VBoxContainer.new()
	box.add_theme_constant_override("separation", 12)
	box.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	box.size_flags_vertical = Control.SIZE_EXPAND_FILL
	_host.add_child(box)

	var title := Label.new()
	title.text = _t("chrome.title", "Customize chrome")
	title.add_theme_font_size_override("font_size", DshTokens.FONT_CHROME_LG)
	title.add_theme_color_override("font_color", DshTokens.text_primary())
	box.add_child(title)

	var hint := Label.new()
	hint.text = _t("chrome.hint", "Rearrange composer widgets. Changes apply when you save.")
	hint.autowrap_mode = TextServer.AUTOWRAP_WORD_SMART
	hint.add_theme_color_override("font_color", DshTokens.text_tertiary())
	hint.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
	box.add_child(hint)

	for spec in DshChromeDocT.SLOTS:
		var short := str(spec["short"])
		var slot_id := str(spec["id"])
		var head := Label.new()
		head.text = _t(str(spec["key"]), str(spec["fallback"]))
		head.add_theme_color_override("font_color", DshTokens.text_secondary())
		box.add_child(head)
		for widget_id in _ids_of(slot_id):
			var row := HBoxContainer.new()
			row.add_theme_constant_override("separation", 4)
			box.add_child(row)
			var name := Label.new()
			name.text = widget_id
			row.add_child(name)
			_fallback_btn(row, "←", "chrome.move.%s.%s.left" % [short, widget_id])
			_fallback_btn(row, "→", "chrome.move.%s.%s.right" % [short, widget_id])
			for other in DshChromeDocT.SLOTS:
				var target := str(other["short"])
				if target == short:
					continue
				_fallback_btn(row, _slot_letter(target),
						"chrome.move.%s.%s.to_%s" % [short, widget_id, target])
			_fallback_btn(row, "×", "chrome.move.%s.%s.out" % [short, widget_id])

	var pal := Label.new()
	pal.text = _t("chrome.customize", "Customize chrome")
	pal.add_theme_color_override("font_color", DshTokens.text_secondary())
	box.add_child(pal)
	for widget_id in _unused_ids():
		var row := HBoxContainer.new()
		row.add_theme_constant_override("separation", 4)
		box.add_child(row)
		var name := Label.new()
		name.text = widget_id
		row.add_child(name)
		for spec in DshChromeDocT.SLOTS:
			_fallback_btn(row, "+", "chrome.add.%s.%s" % [str(spec["short"]), widget_id])

	var actions := HBoxContainer.new()
	actions.add_theme_constant_override("separation", 8)
	box.add_child(actions)
	var spacer := Control.new()
	spacer.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	actions.add_child(spacer)
	_fallback_btn(actions, _t("chrome.reset", "Reset"), "chrome.reset").name = "chrome-reset"
	_fallback_btn(actions, _t("chrome.save", "Save"), "chrome.save").name = "chrome-save"
	_fallback_btn(actions, _t("common.close", "Close"), "chrome.close").name = "chrome-close"


func _fallback_btn(parent: Control, text: String, action_name: String) -> Button:
	var btn := Button.new()
	btn.text = text
	btn.pressed.connect(_on_action.bind(action_name))
	parent.add_child(btn)
	return btn


func _slot_letter(short_slot: String) -> String:
	match short_slot:
		"left":
			return "L"
		"right":
			return "R"
		"overflow":
			return "O"
	return short_slot.substr(0, 1).to_upper()


func _paint_tree(node: Control) -> void:
	_paint_node(node)
	for child in node.get_children():
		if child is Control:
			_paint_tree(child as Control)


func _paint_node(node: Control) -> void:
	if node == null or not is_instance_valid(node):
		return
	if not node.has_meta(DshReconT.META_MODE):
		return
	var hint: Variant = node.get_meta(DshReconT.META_MODE)
	if not (hint is Dictionary):
		return
	var role := str((hint as Dictionary).get("role", ""))
	match role:
		DshChromeDocT.ROLE_TITLE:
			node.add_theme_color_override("font_color", DshTokens.text_primary())
			node.add_theme_font_size_override("font_size", DshTokens.FONT_CHROME_LG)
		DshChromeDocT.ROLE_HINT:
			var hint_lbl := node as Label
			if hint_lbl != null:
				hint_lbl.autowrap_mode = TextServer.AUTOWRAP_WORD_SMART
			node.add_theme_color_override("font_color", DshTokens.text_tertiary())
			node.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
		DshChromeDocT.ROLE_SLOT_TITLE, DshChromeDocT.ROLE_PALETTE_TITLE:
			node.add_theme_color_override("font_color", DshTokens.text_secondary())
			node.add_theme_font_size_override("font_size", DshTokens.FONT_CHROME)
		DshChromeDocT.ROLE_CHIP_LABEL:
			node.add_theme_color_override("font_color", DshTokens.text_primary())
			node.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
		DshChromeDocT.ROLE_EMPTY:
			node.add_theme_color_override("font_color", DshTokens.text_muted())
			node.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
		DshChromeDocT.ROLE_SLOT_PANEL:
			_paint_panel(node as PanelContainer, DshTokens.bg_layer2())
		DshChromeDocT.ROLE_CHIP, DshChromeDocT.ROLE_PALETTE_ITEM:
			_paint_panel(node as PanelContainer, DshTokens.bg_layer3())
		DshChromeDocT.ROLE_MOVE, DshChromeDocT.ROLE_ADD, DshChromeDocT.ROLE_RESET, DshChromeDocT.ROLE_CLOSE:
			_paint_button(node as Button, false)
		DshChromeDocT.ROLE_SAVE:
			_paint_button(node as Button, true)


func _paint_panel(panel: PanelContainer, bg: Color) -> void:
	if panel == null:
		return
	var pad := Vector4(10, 8, 10, 8)
	panel.add_theme_stylebox_override("panel",
			DshTokens.box(bg, DshTokens.RADIUS_MD, DshTokens.border_l1(), 1, pad))


func _paint_button(btn: Button, accent: bool) -> void:
	if btn == null:
		return
	var pad := Vector4(10, 5, 10, 5)
	if accent:
		var rest := DshTokens.box(DshTokens.accent(), DshTokens.RADIUS_MD, Color(0, 0, 0, 0), 0, pad)
		var hov := DshTokens.box(DshTokens.accent_hover(), DshTokens.RADIUS_MD, Color(0, 0, 0, 0), 0, pad)
		btn.add_theme_stylebox_override("normal", rest)
		btn.add_theme_stylebox_override("hover", hov)
		btn.add_theme_stylebox_override("pressed", rest)
		btn.add_theme_color_override("font_color", DshTokens.text_primary())
	else:
		var rest := DshTokens.box(DshTokens.bg_layer2(), DshTokens.RADIUS_MD, DshTokens.border_l1(), 1, pad)
		var hov := DshTokens.box(DshTokens.bg_layer3(), DshTokens.RADIUS_MD, DshTokens.border_l2(), 1, pad)
		btn.add_theme_stylebox_override("normal", rest)
		btn.add_theme_stylebox_override("hover", hov)
		btn.add_theme_stylebox_override("pressed", rest)
		btn.add_theme_color_override("font_color", DshTokens.text_secondary())
	btn.add_theme_font_size_override("font_size", DshTokens.FONT_CAPTION)
	btn.mouse_default_cursor_shape = Control.CURSOR_POINTING_HAND


func _apply_style() -> void:
	if _card != null:
		_card.add_theme_stylebox_override("panel", DshTokens.elevated(
			DshTokens.bg_layer1(),
			DshTokens.RADIUS_LG,
			Vector4(18, 16, 18, 16),
			3
		))


func _on_backdrop(event: InputEvent) -> void:
	if event is InputEventMouseButton:
		var mb := event as InputEventMouseButton
		if mb.pressed and mb.button_index == MOUSE_BUTTON_LEFT:
			close()


func _input(event: InputEvent) -> void:
	if not visible:
		return
	var k := event as InputEventKey
	if k == null or not k.pressed or k.echo:
		return
	if k.keycode == KEY_ESCAPE:
		close()
		get_viewport().set_input_as_handled()


func _t(key: String, fallback: String) -> String:
	var v := str(DshI18n.t(key))
	return fallback if v == key or v.strip_edges() == "" else v
