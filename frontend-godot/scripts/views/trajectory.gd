extends Control
class_name TrajectoryView

## Compact mute timeline. Times come from event.time (unix ms), never wall-clock ticks.

const MAX_EVENTS := 800
const LANE_H := 16.0
const LEDGER_ROW := 16.0
const TIMELINE_FRAC := 0.58
const COL_W: Array[float] = [56.0, 52.0, 88.0]

var _events: Array = []
var _lanes: Array = []
var _zoom: float = 1.0
var _pan: float = 0.0
var _ledger_scroll: int = 0
var _dragging: bool = false
var _drag_start: Vector2 = Vector2.ZERO


func _ready() -> void:
	mouse_filter = Control.MOUSE_FILTER_STOP
	gui_input.connect(_on_gui_input)
	if get_node_or_null("/root/DshI18n") != null:
		DshI18n.locale_changed.connect(_on_locale)


func _on_locale(_loc: String) -> void:
	queue_redraw()


func set_events(events: Array) -> void:
	_events.clear()
	_lanes.clear()
	for raw in events:
		if raw is Dictionary:
			_push(_normalize(raw as Dictionary), false)
	queue_redraw()


func append_event(event: Dictionary) -> void:
	_push(_normalize(event), true)


func clear_all() -> void:
	_events.clear()
	_lanes.clear()
	_zoom = 1.0
	_pan = 0.0
	_ledger_scroll = 0
	queue_redraw()


func _normalize(e: Dictionary) -> Dictionary:
	var t: Variant = e.get("time", e.get("seq", 0))
	var time_ms := int(t)
	var typ := str(e.get("type", e.get("lane", "")))
	var lane := str(e.get("lane", ""))
	if lane == "":
		lane = _lane_for(typ)
	var label := str(e.get("label", ""))
	if label == "":
		label = typ
	var turn := int(e.get("turn", 0))
	return {"time": time_ms, "lane": lane, "label": label, "type": typ, "turn": turn, "seq": int(e.get("seq", 0))}


func _lane_for(typ: String) -> String:
	var t := typ.to_lower()
	if t.begins_with("turn"):
		return "turn"
	if t.begins_with("assistant") or t.contains("/assistant") or t == "assistant":
		return "assistant"
	if t.begins_with("tool") or t.contains("tool-call") or t.contains("tool_call"):
		return "tool"
	if t.contains("result") or t.contains("tool-result"):
		return "result"
	return "other"


func _push(ev: Dictionary, redraw: bool) -> void:
	_events.append(ev)
	var lane := str(ev.get("lane", "other"))
	if not _lanes.has(lane):
		_lanes.append(lane)
	if _events.size() > MAX_EVENTS:
		_events.pop_front()
	if redraw:
		queue_redraw()


func _on_gui_input(event: InputEvent) -> void:
	if event is InputEventMouseButton:
		var mb := event as InputEventMouseButton
		var in_ledger := mb.position.y >= size.y * TIMELINE_FRAC
		if mb.pressed and mb.button_index == MOUSE_BUTTON_WHEEL_UP:
			if in_ledger:
				_ledger_scroll = maxi(0, _ledger_scroll - 1)
			else:
				_zoom = clampf(_zoom * 1.2, 0.5, 3.0)
			queue_redraw()
		elif mb.pressed and mb.button_index == MOUSE_BUTTON_WHEEL_DOWN:
			if in_ledger:
				_ledger_scroll = mini(_ledger_scroll + 1, maxi(0, _events.size() - 1))
			else:
				_zoom = clampf(_zoom / 1.2, 0.5, 3.0)
			queue_redraw()
		elif mb.button_index == MOUSE_BUTTON_LEFT:
			_dragging = mb.pressed
			_drag_start = mb.position
	elif event is InputEventMouseMotion and _dragging:
		var mm := event as InputEventMouseMotion
		var delta: Vector2 = mm.position - _drag_start
		_drag_start = mm.position
		_pan -= delta.x
		queue_redraw()


func _draw() -> void:
	var w := size.x
	var h := size.y
	if w <= 0.0 or h <= 0.0:
		return
	draw_rect(Rect2(0, 0, w, h), DshTokens.bg_code())
	var font := ThemeDB.fallback_font
	if _events.is_empty():
		draw_string(font, Vector2(12, 22), DshI18n.t("trajectory.noActivity"), HORIZONTAL_ALIGNMENT_LEFT, w - 24, 12, DshTokens.text_muted())
		return
	var tl_h := h * TIMELINE_FRAC
	_draw_timeline(w, tl_h, font)
	_draw_ledger(w, h - tl_h, tl_h, font)


func _lane_color(lane: String) -> Color:
	match lane:
		"turn":
			return DshTokens.accent().darkened(0.15)
		"assistant":
			return DshTokens.success().darkened(0.2)
		"tool":
			return DshTokens.warn().darkened(0.15)
		"result":
			return DshTokens.text_secondary()
		_:
			return DshTokens.text_muted()


func _lane_label(lane: String) -> String:
	match lane:
		"turn":
			return DshI18n.t("trajectory.laneTurn")
		"assistant":
			return DshI18n.t("trajectory.laneAssistant")
		"tool":
			return DshI18n.t("trajectory.laneTool")
		"result":
			return DshI18n.t("trajectory.laneResult")
		_:
			return DshI18n.t("trajectory.laneOther")


func _draw_timeline(w: float, tl_h: float, font: Font) -> void:
	var t0 := float(_events[0].get("time", 0))
	var t1 := float(_events[_events.size() - 1].get("time", 0))
	var span := maxf(1.0, t1 - t0)
	var pad := 78.0
	var plot_w := maxf(1.0, w - pad)
	var n := maxi(1, _lanes.size())
	var row_h := maxf(LANE_H + 4.0, tl_h / float(n))
	for li in _lanes.size():
		var lane := str(_lanes[li])
		var y := 6.0 + float(li) * row_h
		draw_string(font, Vector2(6, y + LANE_H - 4), _lane_label(lane), HORIZONTAL_ALIGNMENT_LEFT, pad - 12, 11, DshTokens.text_tertiary())
		for ev in _events:
			if str(ev.get("lane", "")) != lane:
				continue
			var frac := (float(ev.get("time", 0)) - t0 - _pan) / span
			var x := pad + frac * plot_w * _zoom
			var bar_w := maxf(10.0, 5.0 * _zoom)
			if x + bar_w < pad or x > w:
				continue
			var col := _lane_color(lane)
			col.a = 0.75
			draw_rect(Rect2(x, y + 3.0, bar_w, LANE_H - 6.0), col, true)
	draw_line(Vector2(pad, 0), Vector2(pad, tl_h), DshTokens.border_l2())


func _draw_ledger(w: float, ledger_h: float, top: float, font: Font) -> void:
	var header_h := 22.0
	var y := top + header_h
	draw_line(Vector2(0, top), Vector2(w, top), DshTokens.border_l2())
	var headers: Array[String] = ["+ms", "seq", "lane", "event"]
	var cx := 6.0
	for i in 3:
		draw_string(font, Vector2(cx, y - 6), headers[i], HORIZONTAL_ALIGNMENT_LEFT, COL_W[i] - 6, 11, DshTokens.text_muted())
		cx += COL_W[i]
	draw_string(font, Vector2(cx, y - 6), headers[3], HORIZONTAL_ALIGNMENT_LEFT, w - cx - 6, 11, DshTokens.text_muted())
	draw_line(Vector2(0, y), Vector2(w, y), DshTokens.border_l1())
	var t0 := int(_events[0].get("time", 0))
	var idx := _ledger_scroll
	var row_y := y + 2.0
	while idx < _events.size() and row_y + LEDGER_ROW <= top + ledger_h:
		var ev: Dictionary = _events[idx]
		var dt: int = int(ev.get("time", 0)) - t0
		var c := 6.0
		draw_string(font, Vector2(c, row_y + 12), str(dt) + "ms", HORIZONTAL_ALIGNMENT_LEFT, COL_W[0] - 6, 10, DshTokens.text_muted())
		c += COL_W[0]
		draw_string(font, Vector2(c, row_y + 12), str(ev.get("seq", 0)), HORIZONTAL_ALIGNMENT_LEFT, COL_W[1] - 6, 10, DshTokens.text_tertiary())
		c += COL_W[1]
		var lane := str(ev.get("lane", ""))
		draw_string(font, Vector2(c, row_y + 12), _lane_label(lane), HORIZONTAL_ALIGNMENT_LEFT, COL_W[2] - 6, 10, _lane_color(lane))
		c += COL_W[2]
		draw_string(font, Vector2(c, row_y + 12), str(ev.get("label", "")), HORIZONTAL_ALIGNMENT_LEFT, w - c - 6, 10, DshTokens.text_secondary())
		idx += 1
		row_y += LEDGER_ROW
