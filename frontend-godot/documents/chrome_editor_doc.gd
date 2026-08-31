extends RefCounted

## Declarative document for the chrome-layout editor (Phase 3, second engine page).
##
## Built through DshUIDocument.node() using ONLY engine builtins that already
## exist (column/row/text/button/panel/spacer/scroll). Chip/text_input/dropdown
## factories are owned by another agent and must not be assumed here.
##
## Ownership split (same as hero_doc): this file owns the AST, the Reconciler
## owns node identity / prop diffing, DshChromeEditor owns token painting and
## the layout mutation contract. Click stays serializable: String action ids
## dispatched by DshUIReconciler.action.

const DshDocT := preload("res://engine/ui_document.gd")
const DshLayoutT := preload("res://scripts/ui/chrome/layout.gd")

## Visual roles — private contract with DshChromeEditor._paint_tree.
const ROLE_TITLE := "chrome_title"
const ROLE_HINT := "chrome_hint"
const ROLE_SLOT_TITLE := "chrome_slot_title"
const ROLE_SLOT_PANEL := "chrome_slot_panel"
const ROLE_CHIP := "chrome_chip"
const ROLE_CHIP_LABEL := "chrome_chip_label"
const ROLE_MOVE := "chrome_move"
const ROLE_PALETTE_TITLE := "chrome_palette_title"
const ROLE_PALETTE_ITEM := "chrome_palette_item"
const ROLE_ADD := "chrome_add"
const ROLE_SAVE := "chrome_save"
const ROLE_RESET := "chrome_reset"
const ROLE_CLOSE := "chrome_close"
const ROLE_ACTIONS := "chrome_actions"
const ROLE_EMPTY := "chrome_empty"

const ROOT_GAP := 12.0
const SLOT_GAP := 10.0
const CHIP_GAP := 6.0
const ACTION_GAP := 8.0
const BODY_GAP := 14.0

## Short slot keys used inside action ids (no dots, so chrome.move.<slot>.<id>.<dir>
## splits cleanly). Map onto DshChromeLayout slot names.
const SLOTS := [
	{"short": "left", "id": "composer.left", "key": "chrome.slotLeft", "fallback": "Left"},
	{"short": "right", "id": "composer.right", "key": "chrome.slotRight", "fallback": "Right"},
	{"short": "overflow", "id": "composer.overflow", "key": "chrome.slotOverflow", "fallback": "Overflow"},
]


static func build(state: Dictionary) -> Dictionary:
	var slots := _slots_of(state)
	var palette: Array = []
	var raw_palette: Variant = state.get("palette", [])
	if raw_palette is Array:
		palette = raw_palette
	return DshDocT.node("column", {"gap": ROOT_GAP, "expand": true}, [
		DshDocT.node("text", {
			"text": _t("chrome.title", "Customize chrome"),
			"mode": {"role": ROLE_TITLE},
		}, [], "chrome-title"),
		DshDocT.node("text", {
			"text": _t("chrome.hint", "Rearrange composer widgets. Changes apply when you save."),
			"mode": {"role": ROLE_HINT},
		}, [], "chrome-hint"),
		DshDocT.node("scroll", {"expand": true, "height_ratio": 1.0}, [
			DshDocT.node("column", {"gap": BODY_GAP}, [
				_slots_block(slots),
				_palette_block(palette),
			], "chrome-body"),
		], "chrome-scroll"),
		_actions_row(),
	], "chrome-root")


static func _slots_block(slots: Dictionary) -> Dictionary:
	var children: Array = []
	for spec in SLOTS:
		children.append(_slot_panel(spec, slots))
	return DshDocT.node("column", {"gap": SLOT_GAP}, children, "chrome-slots")


static func _slot_panel(spec: Dictionary, slots: Dictionary) -> Dictionary:
	var short := str(spec["short"])
	var slot_id := str(spec["id"])
	var title := _t(str(spec["key"]), str(spec["fallback"]))
	var ids: Array = []
	var raw: Variant = slots.get(slot_id, [])
	if raw is Array:
		ids = raw
	var chips: Array = []
	for item in ids:
		var widget_id := str(item).strip_edges()
		if widget_id != "":
			chips.append(_chip(short, widget_id))
	if chips.is_empty():
		chips.append(DshDocT.node("text", {
			"text": "—",
			"mode": {"role": ROLE_EMPTY},
		}, [], "chrome-slot-%s-empty" % short))
	return DshDocT.node("panel", {
		"padding": 10.0,
		"radius": 8.0,
		"mode": {"role": ROLE_SLOT_PANEL},
	}, [
		DshDocT.node("column", {"gap": CHIP_GAP}, [
			DshDocT.node("text", {
				"text": title,
				"mode": {"role": ROLE_SLOT_TITLE},
			}, [], "chrome-slot-%s-title" % short),
			DshDocT.node("column", {"gap": CHIP_GAP}, chips, "chrome-slot-%s-row" % short),
		], "chrome-slot-%s-body" % short),
	], "chrome-slot-%s" % short)


static func _chip(short_slot: String, widget_id: String) -> Dictionary:
	var hint := "chrome-chip-%s-%s" % [short_slot, widget_id]
	var moves: Array = [
		DshDocT.node("text", {
			"text": widget_id,
			"mode": {"role": ROLE_CHIP_LABEL},
		}, [], hint + "-label"),
		_move_btn(short_slot, widget_id, "left", "←"),
		_move_btn(short_slot, widget_id, "right", "→"),
	]
	for spec in SLOTS:
		var target := str(spec["short"])
		if target == short_slot:
			continue
		var label := _slot_letter(target)
		moves.append(_move_btn(short_slot, widget_id, "to_%s" % target, label,
				_t(str(spec["key"]), str(spec["fallback"]))))
	moves.append(_move_btn(short_slot, widget_id, "out", "×",
			_t("common.remove", "Remove")))
	return DshDocT.node("panel", {
		"padding": 4.0,
		"radius": 6.0,
		"mode": {"role": ROLE_CHIP},
	}, [
		DshDocT.node("row", {"gap": 4.0}, moves, hint + "-row"),
	], hint)


static func _move_btn(short_slot: String, widget_id: String, dir: String, text: String, tooltip: String = "") -> Dictionary:
	var props := {
		"text": text,
		"on_click": "chrome.move.%s.%s.%s" % [short_slot, widget_id, dir],
		"min_width": 28.0,
		"min_height": 26.0,
		"mode": {"role": ROLE_MOVE},
	}
	if tooltip != "":
		props["tooltip"] = tooltip
	return DshDocT.node("button", props, [], "chrome-move-%s-%s-%s" % [short_slot, widget_id, dir])


static func _palette_block(palette: Array) -> Dictionary:
	var items: Array = []
	for item in palette:
		var widget_id := str(item).strip_edges()
		if widget_id != "":
			items.append(_palette_item(widget_id))
	if items.is_empty():
		items.append(DshDocT.node("text", {
			"text": "—",
			"mode": {"role": ROLE_EMPTY},
		}, [], "chrome-palette-empty"))
	return DshDocT.node("column", {"gap": CHIP_GAP}, [
		DshDocT.node("text", {
			"text": _t("chrome.customize", "Customize chrome"),
			"mode": {"role": ROLE_PALETTE_TITLE},
		}, [], "chrome-palette-title"),
		DshDocT.node("column", {"gap": CHIP_GAP}, items, "chrome-palette-row"),
	], "chrome-palette")


static func _palette_item(widget_id: String) -> Dictionary:
	var hint := "chrome-palette-%s" % widget_id
	var adds: Array = [
		DshDocT.node("text", {
			"text": widget_id,
			"mode": {"role": ROLE_CHIP_LABEL},
		}, [], hint + "-label"),
	]
	for spec in SLOTS:
		var short := str(spec["short"])
		adds.append(DshDocT.node("button", {
			"text": "+",
			"tooltip": _t(str(spec["key"]), str(spec["fallback"])),
			"on_click": "chrome.add.%s.%s" % [short, widget_id],
			"min_width": 28.0,
			"min_height": 26.0,
			"mode": {"role": ROLE_ADD},
		}, [], "chrome-add-%s-%s" % [short, widget_id]))
	return DshDocT.node("panel", {
		"padding": 4.0,
		"radius": 6.0,
		"mode": {"role": ROLE_PALETTE_ITEM},
	}, [
		DshDocT.node("row", {"gap": 4.0}, adds, hint + "-row"),
	], hint)


static func _actions_row() -> Dictionary:
	return DshDocT.node("row", {"gap": ACTION_GAP, "mode": {"role": ROLE_ACTIONS}}, [
		DshDocT.node("spacer", {"expand": true}, [], "chrome-actions-lead"),
		DshDocT.node("button", {
			"text": _t("chrome.reset", "Reset"),
			"on_click": "chrome.reset",
			"min_height": 32.0,
			"mode": {"role": ROLE_RESET},
		}, [], "chrome-reset"),
		DshDocT.node("button", {
			"text": _t("chrome.save", "Save"),
			"on_click": "chrome.save",
			"min_height": 32.0,
			"mode": {"role": ROLE_SAVE},
		}, [], "chrome-save"),
		DshDocT.node("button", {
			"text": _t("common.close", "Close"),
			"on_click": "chrome.close",
			"min_height": 32.0,
			"mode": {"role": ROLE_CLOSE},
		}, [], "chrome-close"),
	], "chrome-actions")


static func _slot_letter(short_slot: String) -> String:
	match short_slot:
		"left":
			return "L"
		"right":
			return "R"
		"overflow":
			return "O"
	return short_slot.substr(0, 1).to_upper()


static func _slots_of(state: Dictionary) -> Dictionary:
	var slots: Dictionary = {}
	var raw: Variant = state.get("slots", {})
	if raw is Dictionary:
		slots = (raw as Dictionary).duplicate(true)
	if slots.is_empty():
		slots = DshLayoutT.DEFAULT.duplicate(true)
	return slots


static func _t(key: String, fallback: String) -> String:
	var value := str(DshI18n.t(key))
	return fallback if value == key or value.strip_edges() == "" else value
