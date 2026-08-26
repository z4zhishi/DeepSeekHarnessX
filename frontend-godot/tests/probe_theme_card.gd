extends Node
## Instantiates TerminalBlock/DiffBlock/Plan/Goal/Todo. Fail if THEME_CHANGED
## self-recursion overflows (the GUI freeze when expanding a tool card).
## Run: godot --headless --path . res://tests/probe_theme_card.tscn

func _ready() -> void:
	var scenes := [
		"res://scenes/cards/terminal_block.tscn",
		"res://scenes/cards/diff_block.tscn",
		"res://scenes/cards/plan_card.tscn",
		"res://scenes/cards/goal_card.tscn",
		"res://scenes/cards/todo_card.tscn",
	]
	for path in scenes:
		var ps: PackedScene = load(path)
		if ps == null:
			push_error("THEME_FAIL missing " + path)
			get_tree().quit(1)
			return
		var n: Node = ps.instantiate()
		add_child(n)
		await get_tree().process_frame
		await get_tree().process_frame
	print("THEME_CARD_DONE")
	get_tree().quit(0)
