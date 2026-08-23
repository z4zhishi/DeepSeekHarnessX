extends PanelContainer
class_name ReasoningBox

@onready var toggle_btn: Button = $%ToggleBtn
@onready var content_label: RichTextLabel = $%ReasoningContent
@onready var meta_label: Label = $%MetaLabel

var _expanded: bool = false

func _ready() -> void:
	toggle_btn.pressed.connect(_toggle)

func begin() -> void:
	content_label.text = ""
	meta_label.text = ""
	_expanded = true
	content_label.visible = true
	toggle_btn.text = "Reasoning (click to collapse)"

func append_delta(text: String) -> void:
	content_label.text += text
	content_label.fit_content = true

func finish(elapsed_ms: int, tokens: int) -> void:
	meta_label.text = "%.2fs  %d tokens" % [elapsed_ms / 1000.0, tokens]
	toggle_btn.text = "Reasoning (" + str(tokens) + " tokens)"

func _toggle() -> void:
	_expanded = not _expanded
	content_label.visible = _expanded
	toggle_btn.text = "Hide" if _expanded else "Show"