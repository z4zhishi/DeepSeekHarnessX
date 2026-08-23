# 输入坞：发送按钮 + Enter 提交，草稿队列（↑/↓ 历史回看），
# 多行草稿以 \n 拼接存储。对外暴露 prompt_submitted(text) 信号。
# @ 文件引用做实：attach 按钮弹出目录/文件选择（FileDialog），选中路径以
# @<path> 形式插入 composer；对纯文件名也能正确写入，便于后端按工作区相对路径解析。

extends PanelContainer
class_name InputDock

signal prompt_submitted(text: String)

const MAX_DRAFT_HISTORY := 50

# 兼容旧接线：仍发射，main.gd 可选择性消费（选择器本身已在本控件做实）。
signal file_reference_requested

@onready var prompt_input: LineEdit = $HBox/PromptInput
@onready var send_btn: Button = $HBox/SendBtn
@onready var attach_btn: Button = $HBox/AttachBtn

var _drafts: Array[String] = []
var _draft_index: int = -1
var _file_dialog: FileDialog = null

func _ready() -> void:
	send_btn.pressed.connect(_submit)
	prompt_input.text_submitted.connect(func(_text: String) -> void: _submit())
	prompt_input.gui_input.connect(_on_input)
	attach_btn.pressed.connect(_open_file_picker)

func _open_file_picker() -> void:
	_ensure_file_dialog()
	_file_dialog.popup_centered_ratio(0.7)

func _ensure_file_dialog() -> void:
	if _file_dialog != null:
		return
	_file_dialog = FileDialog.new()
	_file_dialog.file_mode = FileDialog.FILE_MODE_OPEN_FILE
	_file_dialog.access = FileDialog.ACCESS_FILESYSTEM
	_file_dialog.title = "Reference a file (@ insert)"
	_file_dialog.ok_button_text = "Insert path"
	_file_dialog.size = Vector2(560, 420)
	# 可选：从当前文本末尾 @ 后的路径解析起始目录
	var cur := _cursor_anchor_path()
	if cur != "" and DirAccess.dir_exists_absolute(cur):
		_file_dialog.current_dir = cur
	_file_dialog.file_selected.connect(_on_file_selected)
	_file_dialog.canceled.connect(func():
		# 用户取消选择：回退到旧占位信号，保持既有接线不失效
		file_reference_requested.emit()
	)
	add_child(_file_dialog)

func _cursor_anchor_path() -> String:
	# 取光标左侧最近一个 '@' 之后到当前位置的路径前缀，作为 FileDialog 初始目录。
	var text := prompt_input.text
	var caret := prompt_input.caret_column
	var at := text.rfind("@", caret - 1)
	if at == -1:
		return ""
	var tok := text.substr(at + 1, caret - at - 1).strip_edges()
	if tok.is_empty():
		return ""
	return tok

func _on_file_selected(path: String) -> void:
	_pending_path_apply(path)

func _pending_path_apply(path: String) -> void:
	var text := prompt_input.text
	# 移除光标左侧残留的 @token（若存在），再插入 @path
	var caret := prompt_input.caret_column
	var at := text.rfind("@", caret - 1)
	if at != -1:
		text = text.substr(0, at)
		caret = at
	var spacer := ""
	if not text.is_empty() and not text.ends_with(" ") and not text.ends_with("\n"):
		spacer = " "
	text += spacer + "@" + path
	prompt_input.text = text
	prompt_input.caret_column = text.length()
	prompt_input.grab_focus()

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
