extends SceneTree

func _init() -> void:
	var cases := [
		{"w": 320.0, "s": 0.0, "d": 360.0},
		{"w": 800.0, "s": 280.0, "d": 360.0},
		{"w": 1023.0, "s": 280.0, "d": 360.0},
		{"w": 1280.0, "s": 280.0, "d": 360.0},
		{"w": 1600.0, "s": 420.0, "d": 520.0},
	]
	var failures := 0
	for c in cases:
		var cols := DshColumns.compute_columns(c.w, c.s, c.d)
		var total: float = cols.sidebar + cols.center + cols.details
		if total > c.w + 0.1 or cols.center < -0.1 or cols.sidebar < DshColumns.SIDEBAR_COLLAPSED - 0.1:
			failures += 1
			print("FAIL width=%s cols=%s" % [c.w, cols])
		else:
			print("PASS width=%s cols=%s" % [c.w, cols])
	print("COLUMNS_MATRIX_RESULT failures=%d" % failures)
	quit(1 if failures > 0 else 0)
