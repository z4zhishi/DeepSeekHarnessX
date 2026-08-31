extends Node

## Whole-page screenshot probe: captures the real app scene at several window
## sizes in both themes so visual hierarchy can be reviewed by a human.
## PNGs are written to user://screenshots/ (the absolute dir is printed).
## Exits with SCREENCAP_RESULT shots=N failed=M and code 0 on clean saves.

var _failed := 0
var _shot := 0

const SIZES := [
	Vector2i(1440, 900),
	Vector2i(1024, 640),
	Vector2i(960, 600),
]


func _ready() -> void:
	var scene: PackedScene = load("res://scenes/app.tscn")
	if scene == null:
		print("SCREENCAP_RESULT shots=0 failed=1 scene-load-failed")
		get_tree().quit(1)
		return
	var app := scene.instantiate() as Control
	add_child(app)
	await _frames(12)

	var dir := OS.get_user_data_dir().path_join("screenshots")
	DirAccess.make_dir_recursive_absolute(dir)
	for dark in [true, false]:
		app.call("_apply_theme", dark)
		await _frames(12)
		for size in SIZES:
			DisplayServer.window_set_size(size)
			app.get_viewport().size = Vector2i(size.x, size.y)
			await _frames(14)
			# Headless 下 RenderingServer 可能没有可用纹理，先判 null 再取图。
			if app.get_viewport().get_texture() == null:
				print("SHOT-SKIP (no texture) %dx%d" % [size.x, size.y])
				await _frames(6)
				continue
			var img := app.get_viewport().get_texture().get_image()
			if img == null:
				print("SHOT-SKIP (null image) %dx%d" % [size.x, size.y])
				await _frames(6)
				continue
			var name := "page_%s_%dx%d.png" % [str("dark") if dark else "light", size.x, size.y]
			var err := img.save_png("user://screenshots/" + name)
			if err != OK:
				_failed += 1
				print("FAIL: save %s err=%d" % [name, err])
			else:
				print("SHOT %s" % name)
			_shot += 1
			await _frames(6)
	print("USER_DIR %s" % OS.get_user_data_dir())
	print("SCREENCAP_RESULT shots=%d failed=%d" % [_shot, _failed])
	get_tree().quit(1 if _failed > 0 else 0)


func _frames(n: int) -> void:
	for _i in n:
		await get_tree().process_frame