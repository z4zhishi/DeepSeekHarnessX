# 轨迹画布：以事件流（session/event 的 turn/start、tool/call、turn/end）
# 绘制时间线条带。支持滚轮垂直缩放（0.5x-3x）与拖拽平移。
# 纯 CanvasItem _draw() 绘制，无节点开销。

extends Control
class_name TrajectoryCanvas

var _turns: Array[Dictionary] = []  # {start: int, end: int, label: String}
var _zoom: float = 1.0
var _pan: float = 0.0
var _dragging: bool = false
var _drag_start_y: float = 0.0

func _ready() -> void:
	gui_input.connect(_on_gui_input)
	mouse_filter = Control.MOUSE_FILTER_STOP

func record_turn_start(turn: int) -> void:
	_turns.append({"start": Time.get_ticks_msec(), "end": 0, "label": "Turn " + str(turn)})
	queue_redraw()

func record_event(label: String) -> void:
	if _turns.is_empty():
		return
	_turns[_turns.size() - 1]["label"] = label
	queue_redraw()

func record_turn_end() -> void:
	if _turns.is_empty():
		return
	_turns[_turns.size() - 1]["end"] = Time.get_ticks_msec()
	queue_redraw()

func clear_all() -> void:
	_turns.clear()
	_zoom = 1.0
	_pan = 0.0
	queue_redraw()

func _on_gui_input(event: InputEvent) -> void:
	if event is InputEventMouseButton:
		if event.button_index == MOUSE_BUTTON_WHEEL_UP and event.pressed:
			_zoom = clampf(_zoom * 1.2, 0.5, 3.0)
			queue_redraw()
		elif event.button_index == MOUSE_BUTTON_WHEEL_DOWN and event.pressed:
			_zoom = clampf(_zoom / 1.2, 0.5, 3.0)
			queue_redraw()
		elif event.button_index == MOUSE_BUTTON_LEFT:
			_dragging = event.pressed
			_drag_start_y = event.position.y
	elif event is InputEventMouseMotion and _dragging:
		_pan += event.position.y - _drag_start_y
		_drag_start_y = event.position.y
		queue_redraw()

func _draw() -> void:
	var w := size.x
	var h := size.y
	if w <= 0 or h <= 0:
		return
	draw_rect(Rect2(0, 0, w, h), Color(0.05, 0.06, 0.08, 0.6), true)
	if _turns.is_empty():
		draw_string(ThemeDB.fallback_font, Vector2(12, 20), "No activity yet (wheel: zoom, drag: pan)", HORIZONTAL_ALIGNMENT_LEFT, w - 24, 12, Color(0.5, 0.5, 0.5))
		return
	var bar_h := 8.0
	var gap := 4.0
	for i in _turns.size():
		var t: Dictionary = _turns[i]
		var y := 12.0 + i * (bar_h + gap) + _pan
		if y < -20 or y > h + 20:
			continue
		var start: int = t.get("start", 0)
		var end: int = t.get("end", Time.get_ticks_msec())
		var dur := maxi(1, end - start)
		var frac := clampf(dur / 12000.0, 0.06, 1.0) * _zoom
		var bar_w := clampf(w * frac, 6.0, w)
		draw_rect(Rect2(0, y, bar_w, bar_h), Color(0.3, 0.7, 0.9, 0.85), true)
		draw_string(ThemeDB.fallback_font, Vector2(bar_w + 8, y + bar_h), str(t.get("label", "")), HORIZONTAL_ALIGNMENT_LEFT, w - bar_w - 16, 11, Color(0.8, 0.85, 0.9))