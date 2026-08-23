# 轨迹画布（Chrome-network 风格）：多 lane 事件时间轴 + 底部 ledger 表。
# 保留旧 API（record_turn_start / record_turn_end / record_event / clear_all）以兼容
# main.gd 的调用，但内部把每个事件按时间序列沉淀、按 lane 分轨绘制，
# 而非"只显示最后一个 label"。
# 纯 CanvasItem _draw() 绘制，无节点开销。
# 交互：时间轴区滚轮水平缩放（0.5x-3x）、拖拽平移；ledger 区滚轮滚动表格。

extends Control
class_name TrajectoryCanvas

const LANE_COLORS := {
	"turn": Color(0.38, 0.55, 0.90),
	"assistant": Color(0.32, 0.82, 0.48),
	"tool": Color(0.90, 0.62, 0.28),
	"result": Color(0.60, 0.48, 0.88),
	"other": Color(0.52, 0.52, 0.58),
}
const MAX_EVENTS: int = 800
const TIMELINE_FRAC: float = 0.55
const LANE_H: float = 18.0
const LEDGER_ROW_H: float = 17.0
const LEDGER_COL_W: Array[float] = [60.0, 60.0, 110.0]

var _events: Array[Dictionary] = []   # {time:int, lane:String, label:String, turn:int}
var _lane_order: Array[String] = []   # lane 首次出现顺序
var _current_turn: int = 0
var _zoom: float = 1.0                # 时间轴水平缩放
var _pan: float = 0.0                 # 时间轴水平平移（ms）
var _ledger_scroll: int = 0           # ledger 表跳过的顶部行数
var _dragging: bool = false
var _drag_start: Vector2 = Vector2.ZERO

func _ready() -> void:
	gui_input.connect(_on_gui_input)
	mouse_filter = Control.MOUSE_FILTER_STOP

func record_turn_start(turn: int) -> void:
	_current_turn = turn
	_push_event("turn", "Turn " + str(turn) + " start", turn)

func record_turn_end() -> void:
	_push_event("turn", "Turn end", _current_turn)

func record_event(label: String) -> void:
	_push_event(_classify_lane(label), label, _current_turn)

func clear_all() -> void:
	_events.clear()
	_lane_order.clear()
	_current_turn = 0
	_zoom = 1.0
	_pan = 0.0
	_ledger_scroll = 0
	queue_redraw()

func _classify_lane(label: String) -> String:
	if label.begins_with("tool"):
		return "tool"
	if label == "assistant":
		return "assistant"
	if label == "result":
		return "result"
	if label.begins_with("Turn") or label.begins_with("turn"):
		return "turn"
	return "other"

func _push_event(lane: String, label: String, turn: int) -> void:
	_events.append({"time": Time.get_ticks_msec(), "lane": lane, "label": label, "turn": turn})
	if not _lane_order.has(lane):
		_lane_order.append(lane)
	if _events.size() > MAX_EVENTS:
		_events.pop_front()
	queue_redraw()

func _on_gui_input(event: InputEvent) -> void:
	if event is InputEventMouseButton:
		var in_ledger: bool = event.position.y >= size.y * TIMELINE_FRAC
		if event.pressed and event.button_index == MOUSE_BUTTON_WHEEL_UP:
			if in_ledger:
				_ledger_scroll = maxi(0, _ledger_scroll - 1)
			else:
				_zoom = clampf(_zoom * 1.2, 0.5, 3.0)
			queue_redraw()
		elif event.pressed and event.button_index == MOUSE_BUTTON_WHEEL_DOWN:
			if in_ledger:
				_ledger_scroll += 1
				_ledger_scroll = mini(_ledger_scroll, maxi(0, _events.size() - 1))
			else:
				_zoom = clampf(_zoom / 1.2, 0.5, 3.0)
			queue_redraw()
		elif event.button_index == MOUSE_BUTTON_LEFT:
			_dragging = event.pressed
			_drag_start = event.position
	elif event is InputEventMouseMotion and _dragging:
		var delta: Vector2 = event.position - _drag_start
		_drag_start = event.position
		_pan -= delta.x
		queue_redraw()

func _draw() -> void:
	var w := size.x
	var h := size.y
	if w <= 0 or h <= 0:
		return
	draw_rect(Rect2(0, 0, w, h), Color(0.05, 0.06, 0.08, 0.6), true)
	if _events.is_empty():
		draw_string(ThemeDB.fallback_font, Vector2(12, 20), "No activity yet (wheel: zoom / ledger scroll, drag: pan)", HORIZONTAL_ALIGNMENT_LEFT, w - 24, 12, Color(0.5, 0.5, 0.5))
		return
	var tl_h := h * TIMELINE_FRAC
	_draw_timeline(w, tl_h)
	_draw_ledger(w, h - tl_h, tl_h)

func _lane_color(lane: String) -> Color:
	return LANE_COLORS.get(lane, LANE_COLORS["other"])

func _draw_timeline(w: float, tl_h: float) -> void:
	var t0 := float(_events[0].get("time", 0))
	var t1 := float(_events[_events.size() - 1].get("time", 0))
	var span := maxf(1.0, t1 - t0)
	var time_pad := 78.0
	var plot_w := maxf(1.0, w - time_pad)
	var n_lanes := _lane_order.size()
	var row_h := maxf(LANE_H + 6.0, tl_h / float(n_lanes))
	for li in n_lanes:
		var lane := _lane_order[li]
		var y := 6.0 + li * row_h
		draw_string(ThemeDB.fallback_font, Vector2(6, y + LANE_H - 4), lane, HORIZONTAL_ALIGNMENT_LEFT, time_pad - 12, 12, Color(0.72, 0.76, 0.82))
		for i in _events.size():
			var ev: Dictionary = _events[i]
			if ev.get("lane", "") != lane:
				continue
			var frac := (float(ev.get("time", 0)) - t0 - _pan) / span
			var x := time_pad + frac * plot_w * _zoom
			var bar_w := maxf(14.0, 6.0 * _zoom)
			if x + bar_w < time_pad or x > w:
				continue
			draw_rect(Rect2(x, y + 3.0, bar_w, LANE_H - 5.0), _lane_color(lane), true)
	# 时间轴竖线分隔 lane 标签与事件区
	draw_line(Vector2(time_pad, 0), Vector2(time_pad, tl_h), Color(0.28, 0.28, 0.32))

func _draw_ledger(w: float, ledger_h: float, top: float) -> void:
	var header_h := 26.0
	var y := top + header_h
	# 分隔线
	draw_line(Vector2(0, top), Vector2(w, top), Color(0.3, 0.3, 0.35))
	# 表头
	var header_labels: Array[String] = ["+ms", "Turn", "Lane", "Event"]
	var cx := 6.0
	for i in 3:
		draw_string(ThemeDB.fallback_font, Vector2(cx, y - 6), header_labels[i], HORIZONTAL_ALIGNMENT_LEFT, LEDGER_COL_W[i] - 6, 12, Color(0.6, 0.65, 0.7))
		cx += LEDGER_COL_W[i]
	draw_string(ThemeDB.fallback_font, Vector2(cx, y - 6), header_labels[3], HORIZONTAL_ALIGNMENT_LEFT, w - cx - 6, 12, Color(0.6, 0.65, 0.7))
	draw_line(Vector2(0, y), Vector2(w, y), Color(0.3, 0.3, 0.35))
	# 事件行（按时间顺序，从 _ledger_scroll 起）
	var t0 := float(_events[0].get("time", 0))
	var idx := _ledger_scroll
	var row_y := y + 2.0
	while idx < _events.size() and row_y + LEDGER_ROW_H <= top + ledger_h:
		var ev: Dictionary = _events[idx]
		var dt: int = int(ev.get("time", 0)) - int(t0)
		var c := 6.0
		draw_string(ThemeDB.fallback_font, Vector2(c, row_y + 12), str(dt) + "ms", HORIZONTAL_ALIGNMENT_LEFT, LEDGER_COL_W[0] - 6, 11, Color(0.52, 0.56, 0.62))
		c += LEDGER_COL_W[0]
		draw_string(ThemeDB.fallback_font, Vector2(c, row_y + 12), str(ev.get("turn", 0)), HORIZONTAL_ALIGNMENT_LEFT, LEDGER_COL_W[1] - 6, 11, Color(0.6, 0.6, 0.7))
		c += LEDGER_COL_W[1]
		draw_string(ThemeDB.fallback_font, Vector2(c, row_y + 12), str(ev.get("lane", "")), HORIZONTAL_ALIGNMENT_LEFT, LEDGER_COL_W[2] - 6, 11, _lane_color(str(ev.get("lane", ""))))
		c += LEDGER_COL_W[2]
		draw_string(ThemeDB.fallback_font, Vector2(c, row_y + 12), str(ev.get("label", "")), HORIZONTAL_ALIGNMENT_LEFT, w - c - 6, 11, Color(0.85, 0.88, 0.92))
		idx += 1
		row_y += LEDGER_ROW_H
