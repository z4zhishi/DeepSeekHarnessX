# 输入坞：发送按钮 + Enter 提交，草稿队列（↑/↓ 历史回看），
# 多行草稿以 \n 拼接存储。对外暴露 prompt_submitted(text) 信号。
# @ 文件引用做实：attach 按钮弹出目录/文件选择（FileDialog），选中路径以
# @<path> 形式插入 composer；对纯文件名也能正确写入，便于后端按工作区相对路径解析。
#
# B2 slash 命令弹层：输入 / 弹出命令列表（plan/permission/feedback/exit/help，
# 与后端 session.command 已注册命令一致），选择后填充 prompt 供 Enter 发送。
# B3 attachment：composer 上方图片 rail；选择/拖拽图片（PNG/JPG/WEBP）后以
# 缩略图显示，并把 @<path> 拼进待发 prompt（后端按工作区相对路径解析图片）。

extends PanelContainer
class_name InputDock

signal prompt_submitted(text: String)

const MAX_DRAFT_HISTORY := 50

# 兼容旧接线：仍发射，main.gd 可选择性消费（选择器本身已在本控件做实）。
signal file_reference_requested

# 前端静态命令列表（与后端 CommandRegistry.RegisterBuiltinCommands 注册集一致）
const SLASH_COMMANDS: Array[Dictionary] = [
	{"name": "plan", "desc": "Enter or leave plan mode; /plan <message> records the plan."},
	{"name": "permission", "desc": "Apply permission preset: default | strict | unrestricted."},
	{"name": "feedback", "desc": "Record feedback; /feedback <text> appends a feedback/record event."},
	{"name": "exit", "desc": "Exit the interactive session."},
	{"name": "help", "desc": "List available commands."},
]

const IMAGE_EXT: Array[String] = ["png", "jpg", "jpeg", "webp", "bmp"]

@onready var prompt_input: LineEdit = $VBox/HBox/PromptInput
@onready var send_btn: Button = $VBox/HBox/SendBtn
@onready var attach_btn: Button = $VBox/HBox/AttachBtn
@onready var attach_rail: HBoxContainer = $VBox/AttachRail

var _drafts: Array[String] = []
var _draft_index: int = -1
var _file_dialog: FileDialog = null

# B2 命令弹层
var _cmd_popup: PopupMenu = null
var _cmd_dirty: bool = false

# B3 attachment：rail 内挂缩略图容器，每张图 = HBox(缩略图 TextureRect + 移除 btn)
var _attachments: Array[Dictionary] = []  # [{path, node}]

func _ready() -> void:
	send_btn.pressed.connect(_submit)
	prompt_input.text_submitted.connect(func(_text: String) -> void: _submit())
	prompt_input.gui_input.connect(_on_input)
	attach_btn.pressed.connect(_open_file_picker)
	# 拖拽释放到整坞接受图片文件（B3）
	gui_input.connect(_on_dock_gui_input)
	_ensure_cmd_popup()

# ---------------- slash 命令弹层（B2） ----------------

func _ensure_cmd_popup() -> void:
	if _cmd_popup != null:
		return
	_cmd_popup = PopupMenu.new()
	_cmd_popup.id_pressed.connect(_on_cmd_selected)
	_cmd_popup.about_to_popup.connect(_refresh_cmd_menu)
	# 挂在 PromptInput 下以便定位到输入框旁
	prompt_input.add_child(_cmd_popup)

func _refresh_cmd_menu() -> void:
	if not _cmd_dirty:
		return
	_cmd_dirty = false
	_cmd_popup.clear()
	for i in SLASH_COMMANDS.size():
		var cmd: Dictionary = SLASH_COMMANDS[i]
		_cmd_popup.add_item("/" + str(cmd.get("name", "")), i)
	_cmd_popup.set_item_tooltip(0, str(SLASH_COMMANDS[0].get("desc", "")))

func _on_cmd_selected(id: int) -> void:
	if id < 0 or id >= SLASH_COMMANDS.size():
		return
	var name: String = str(SLASH_COMMANDS[id].get("name", ""))
	# 命令名填充输入框（含参数可在行尾续写），供用户确认后 Enter 发送
	prompt_input.text = "/" + name + " "
	prompt_input.caret_column = prompt_input.text.length()
	prompt_input.grab_focus()

# ---------------- attachment 图片（B3） ----------------

func _open_file_picker() -> void:
	_ensure_file_dialog()
	_file_dialog.popup_centered_ratio(0.7)

func _ensure_file_dialog() -> void:
	if _file_dialog != null:
		return
	_file_dialog = FileDialog.new()
	_file_dialog.file_mode = FileDialog.FILE_MODE_OPEN_FILES
	_file_dialog.access = FileDialog.ACCESS_FILESYSTEM
	_file_dialog.title = "Attach image(s)"
	_file_dialog.ok_button_text = "Attach"
	_file_dialog.size = Vector2(560, 420)
	# 图片过滤模式
	_file_dialog.add_filter("*.png, *.jpg, *.jpeg, *.webp, *.bmp ; Images")
	var cur := _cursor_anchor_path()
	if cur != "" and DirAccess.dir_exists_absolute(cur):
		_file_dialog.current_dir = cur
	_file_dialog.files_selected.connect(_on_files_selected)
	_file_dialog.canceled.connect(func():
		file_reference_requested.emit()
	)
	add_child(_file_dialog)

func _on_files_selected(paths: PackedStringArray) -> void:
	for p in paths:
		if _is_image(p):
			_attach_image(p)
		else:
			# 非图片文件仍走 @ 文本引用插入（向后兼容旧 AttachBtn 行为）
			_insert_ref(p)

func _is_image(path: String) -> bool:
	var ext := path.get_extension().to_lower()
	return ext in IMAGE_EXT

func _attach_image(path: String) -> void:
	for a in _attachments:
		if str(a.get("path", "")) == path:
			return  # 去重
	var row := HBoxContainer.new()
	var thumb := TextureRect.new()
	var img := Image.load_from_file(path)
	if img != null:
		var tex := ImageTexture.create_from_image(img)
		thumb.texture = tex
	thumb.custom_minimum_size = Vector2(36, 36)
	thumb.stretch_mode = TextureRect.STRETCH_KEEP_ASPECT_CENTERED
	row.add_child(thumb)
	var label := Label.new()
	label.text = path.get_file()
	label.add_theme_font_size_override("font_size", 12)
	row.add_child(label)
	var remove_btn := Button.new()
	remove_btn.text = "x"
	remove_btn.flat = true
	remove_btn.pressed.connect(func(): _remove_attachment(row, path))
	row.add_child(remove_btn)
	attach_rail.add_child(row)
	_attachments.append({"path": path, "node": row})
	# 图片引用也以 @<path> 形式拼进 prompt（后端按工作区相对路径解析）
	_insert_ref(path)

func _remove_attachment(row: Control, path: String) -> void:
	for i in _attachments.size():
		if str(_attachments[i].get("path", "")) == path:
			_attachments.remove_at(i)
			break
	if is_instance_valid(row):
		row.queue_free()

# 插入 @<path> 到 prompt 光标处
func _insert_ref(path: String) -> void:
	var text := prompt_input.text
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

# 拖拽释放：整个坞接受图片文件
func _on_dock_gui_input(event: InputEvent) -> void:
	if event != null and event.get_class() == "InputEventFilesDropped":
		var files: Array = event.get("files")
		for p in files:
			if _is_image(str(p)):
				_attach_image(str(p))
			else:
				_insert_ref(str(p))
		event.accept()

func _cursor_anchor_path() -> String:
	var text := prompt_input.text
	var caret := prompt_input.caret_column
	var at := text.rfind("@", caret - 1)
	if at == -1:
		return ""
	var tok := text.substr(at + 1, caret - at - 1).strip_edges()
	if tok.is_empty():
		return ""
	return tok

func _submit() -> void:
	var text := prompt_input.text.strip_edges()
	if text == "":
		return
	_push_draft(text)
	prompt_input.text = ""
	_clear_rail()
	prompt_submitted.emit(text)

func _clear_rail() -> void:
	_attachments.clear()
	for c in attach_rail.get_children():
		attach_rail.remove_child(c)
		c.queue_free()

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
		if event.keycode == KEY_SLASH:
			# 已输入 "/" 才弹命令层
			if prompt_input.text == "/":
				_cmd_dirty = true
				_cmd_popup.popup(Rect2i(prompt_input.global_position, Vector2(260, 0)))
				event.accept()
				return
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
