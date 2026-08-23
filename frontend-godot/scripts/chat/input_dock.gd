# 输入坞：发送按钮 + Enter 提交，草稿队列（↑/↓ 历史回看），
# 多行草稿以 \n 拼接存储。对外暴露 prompt_submitted(text) 信号。

extends PanelContainer
class_name InputDock

signal prompt_submitted(text: String)

const MAX_DRAFT_HISTORY := 50

signal file_reference_requested

@onready var prompt_input: LineEdit = $HBox/PromptInput
@onready var send_btn: Button = $HBox/SendBtn
@onready var attach_btn: Button = $HBox/AttachBtn

var _drafts: Array[String] = []
var _draft_index: int = -1

func _ready() -> void:
	send_btn.pressed.connect(_submit)
	prompt_input.text_submitted.connect(func(_text: String) -> void: _submit())
	prompt_input.gui_input.connect(_on_input)
	attach_btn.pressed.connect(func() -> void: file_reference_requested.emit())

func _submit() -> void:
	var text := prompt_input.text.strip_edges()
	if text == "":
		return
	_push_draft(text)
	prompt_input.text = ""
	prompt_submitted.emit(text)

func _push_draft(text: String) -> void:
	if not _drafts.is_empty() and _drafts[_drafts.size() - 1] == text:
		_drafts[_drafts.size() - 1] = text
		return
	_drafts.append(text)
	if _drafts.size() > MAX_DRAFT_HISTORY:
		_drafts.pop_front()
	_draft_index = _drafts.size()

func _on_input(event: InputEvent) -> void:
	if event is InputEventKey and event.pressed and not event.echo:
		if event.keycode == KEY_UP:
			if _draft_index > 0:
				_draft_index -= 1
				prompt_input.text = _drafts[_draft_index]
				prompt_input.caret_column = prompt_input.text.length()
				prompt_input.accept_event()
		elif event.keycode == KEY_DOWN:
			if _draft_index < _drafts.size():
				_draft_index += 1
				if _draft_index < _drafts.size():
					prompt_input.text = _drafts[_draft_index]
				else:
					prompt_input.text = ""
				prompt_input.caret_column = prompt_input.text.length()
				prompt_input.accept_event()

func set_placeholder(text: String) -> void:
	prompt_input.placeholder_text = text