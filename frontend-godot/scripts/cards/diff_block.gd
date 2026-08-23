extends PanelContainer
class_name DiffBlock

@onready var text_label: RichTextLabel = $%DiffText
@onready var header_label: Label = $%DiffHeader
@onready var toggle_btn: Button = $%ToggleBtn

var _expanded: bool = true

func _ready() -> void:
	toggle_btn.pressed.connect(_toggle)

func setup(diff_text: String, title: String = "Diff") -> void:
	header_label.text = title
	text_label.text = _build(diff_text)
	text_label.fit_content = true

func _toggle() -> void:
	_expanded = not _expanded
	text_label.visible = _expanded
	toggle_btn.text = "Collapse" if _expanded else "Expand"

func _build(diff_text: String) -> String:
	var bb := ""
	var in_hunk := false
	for line in diff_text.split("\n"):
		if line.begins_with("+++") or line.begins_with("---") or line.begins_with("@@"):
			bb += "[color=#b0bec5]" + line.replace("[", "[lb]").replace("]", "[/lb]") + "[/color]\n"
			in_hunk = true
		elif line.begins_with("+") and in_hunk:
			bb += "[color=#81c784]" + line.replace("[", "[lb]").replace("]", "[/lb]") + "[/color]\n"
		elif line.begins_with("-") and in_hunk:
			bb += "[color=#e57373]" + line.replace("[", "[lb]").replace("]", "[/lb]") + "[/color]\n"
		else:
			bb += line.replace("[", "[lb]").replace("]", "[/lb]") + "\n"
	return bb