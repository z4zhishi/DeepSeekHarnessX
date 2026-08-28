extends Node
## Responsive column matrix probe. Uses real window/viewport sizing and GUI input.

var _t0 := 0
var _passed := 0
var _failed := 0
var _signal_count := 0

func _stamp(m: String) -> void:
	var line := "%8.3fs f=%d %s" % [(Time.get_ticks_msec() - _t0) / 1000.0, Engine.get_process_frames(), m]
	var f := FileAccess.open("user://toggle_log.txt", FileAccess.READ_WRITE)
	if f == null:
		f = FileAccess.open("user://toggle_log.txt", FileAccess.WRITE)
	if f:
		f.seek_end()
		f.store_line(line)
		f.close()
	print(line)

func _assert(cond: bool, msg: String) -> void:
	if cond:
		_passed += 1
		_stamp("PASS: " + msg)
	else:
		_failed += 1
		_stamp("FAIL: " + msg)

func _ready() -> void:
	_t0 = Time.get_ticks_msec()
	var f := FileAccess.open("user://toggle_log.txt", FileAccess.WRITE)
	if f:
		f.store_line("=== responsive toggle probe start ===")
	_run()

func _wait_layout(frames: int = 6) -> void:
	for _i in frames:
		await get_tree().process_frame

func _click(viewport: Viewport, point: Vector2) -> void:
	var down := InputEventMouseButton.new()
	down.button_index = MOUSE_BUTTON_LEFT
	down.pressed = true
	down.position = point
	down.global_position = point
	viewport.push_input(down)
	await get_tree().process_frame
	var up := InputEventMouseButton.new()
	up.button_index = MOUSE_BUTTON_LEFT
	up.pressed = false
	up.position = point
	up.global_position = point
	viewport.push_input(up)
	await get_tree().process_frame

func _rects(app: Control) -> Dictionary:
	return {
		"app": app.get_global_rect(),
		"frame": app.get_node("Frame").get_global_rect(),
		"slot": app._sidebar_slot.get_global_rect(),
		"sidebar": app._sidebar.get_global_rect(),
		"center": app._center.get_global_rect(),
		"composer": app._composer.get_global_rect(),
		"details": app._details.get_global_rect(),
	}

func _probe_geometry(app: Control, label: String) -> void:
	var r := _rects(app)
	var vp := app.get_viewport().get_visible_rect()
	_stamp("RECTS %s app=%s frame=%s slot=%s sidebar=%s center=%s composer=%s details=%s viewport=%s" % [label, str(r.app), str(r.frame), str(r.slot), str(r.sidebar), str(r.center), str(r.composer), str(r.details), str(vp)])
	var button := app._sidebar._collapse as Control
	var br := button.get_global_rect()
	_assert(br.size.x > 0.0 and br.size.y > 0.0, label + " collapse button has geometry")
	_assert(vp.has_point(br.get_center()), label + " collapse button center is in viewport")
	_assert(r.slot.intersects(br) and r.sidebar.intersects(br), label + " collapse button is in sidebar/slot")
	_assert(r.center.size.x >= 0.0 and r.center.size.y >= 0.0, label + " center has valid size")
	_assert(vp.has_point(r.center.get_center()), label + " primary columns are inside viewport")
	if app._details.visible:
		_assert(vp.encloses(r.details), label + " visible details is inside viewport")
		_assert(not r.center.intersects(r.details), label + " center and details do not overlap")
	else:
		_assert(not app._details.visible, label + " details hidden when space is insufficient")
	_assert(r.sidebar.size.x <= r.slot.size.x + 1.0, label + " sidebar cannot widen its slot")

func _set_size(size: Vector2i) -> void:
	DisplayServer.window_set_size(size)
	get_viewport().size = size
	await _wait_layout(8)

func _run() -> void:
	var scene: PackedScene = load("res://scenes/app.tscn")
	var app := scene.instantiate() as Control
	add_child(app)
	var viewport := get_viewport()
	var sizes := [Vector2i(960, 600), Vector2i(980, 600), Vector2i(1023, 600), Vector2i(1024, 600), Vector2i(1280, 800), Vector2i(1440, 900), Vector2i(1920, 1080)]
	await _wait_layout(10)
	app._sidebar.collapse_pressed.connect(func() -> void: _signal_count += 1)
	for size in sizes:
		await _set_size(size)
		app._apply_columns()
		await _wait_layout(8)
		_probe_geometry(app, "%dx%d initial" % [size.x, size.y])

		# Real input path: click the actual button twice and verify signal/state closure.
		var first := (app._sidebar._collapse as Control).get_global_rect().get_center()
		var before := _signal_count
		await _click(viewport, first)
		await _wait_layout(8)
		_assert(_signal_count == before + 1, "%dx%d first push_input emits exactly one signal" % [size.x, size.y])
		_probe_geometry(app, "%dx%d after first click" % [size.x, size.y])
		var second := (app._sidebar._collapse as Control).get_global_rect().get_center()
		before = _signal_count
		await _click(viewport, second)
		await _wait_layout(8)
		_assert(_signal_count == before + 1, "%dx%d second push_input emits exactly one signal" % [size.x, size.y])
		_probe_geometry(app, "%dx%d after second click" % [size.x, size.y])

	_stamp("MATRIX_RESULT passed=%d failed=%d signal_count=%d" % [_passed, _failed, _signal_count])
	print("PROBE_SUMMARY passed=%d failed=%d signal_count=%d" % [_passed, _failed, _signal_count])
	get_viewport().size = Vector2i(800, 600)
	await _wait_layout(6)
	call_deferred("_quit", 1 if _failed > 0 else 0)


func _quit(code: int) -> void:
	get_tree().quit(code)
