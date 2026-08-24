extends ScrollContainer
class_name ChatView

## 真虚拟滚动聊天视图：按视口裁剪 + 复用节点，替换原"全 append + NODE_POOL_SIZE 死常量"。
##
## 设计：唯一子节点 _content（Control），按 y 绝对定位挂载卡片。
## 每次同步只把落在视口窗口（含 OVERSCAN 缓冲）内的卡片 add_child 进 _content，
## 其余卡片留在逻辑 _items/_heights 数组（detached 节点，不参与布局不绘制），
## 因此挂载节点数 ~O(视口高/卡片高)，与消息总数无关，长会话滚动不卡。
## 卡片节点一直保留在 _items 中（只挂/卸，不销毁），满足"复用节点"要求。
##
## 对外 API 与 main.gd 现有调用兼容：
##   add_message(role,text[,message_id]) / add_card(card) / append_streaming(delta)
##   end_streaming() / clear_messages()
##
## message-feedback（B1）：add_message("assistant", text, messageId) 会为消息卡
## 附加 like/dislike + note 控制条；按钮经 feedback_rating / feedback_note 信号
## 交由 main.gd 接后端 feedback.put RPC。messageId 存于卡片 set_meta。

signal feedback_rating(message_id: String, rating: String)
signal feedback_note(message_id: String, note: String)

const OVERSCAN := 80
const CHAR_H := 18
const PAD := 16
const MIN_H := 40
const FOOTER_H := 30

var _content: Control
var _items: Array = []            # Array[Control]
var _heights: Array = []          # Array[int]
var _mounted: Array = []          # Array[int]：当前挂载的 _items 下标
var _sync_pending: bool = false
var _last_stream_idx: int = -1
var _stream_text: String = ""

func _ready() -> void:
	_content = Control.new()
	_content.name = "Content"
	_content.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_content.size_flags_vertical = Control.SIZE_EXPAND_FILL
	_content.mouse_filter = Control.MOUSE_FILTER_PASS
	add_child(_content)

	get_v_scroll_bar().value_changed.connect(func(_v: float): _request_sync())
	resized.connect(func(): _request_sync())
	call_deferred("_sync")

## ---------------- 对外 API ----------------

func add_message(role: String, text: String, message_id: String = "") -> void:
	var card := _new_text_card(role, text)
	if message_id != "":
		card.set_meta("message_id", message_id)
	_append_item(card)

func add_card(card: Control) -> void:
	_append_item(card)

func append_streaming(delta: String) -> void:
	if _last_stream_idx < 0:
		var card := _new_text_card("assistant", "")
		_append_item(card)
		_last_stream_idx = _items.size() - 1
		_stream_text = ""
	_stream_text += delta
	var lbl: RichTextLabel = _find_body(_items[_last_stream_idx])
	lbl.text = _bb_assistant(_stream_text)
	_heights[_last_stream_idx] = _estimate_height(_items[_last_stream_idx])
	_request_sync()

func end_streaming() -> void:
	_last_stream_idx = -1
	_stream_text = ""

## B1：流式 assistant 消息补记 message id（feedback 按钮按 id 定位）。
func set_last_assistant_message_id(message_id: String) -> void:
	if _last_stream_idx < 0 or _last_stream_idx >= _items.size():
		return
	_items[_last_stream_idx].set_meta("message_id", message_id)
	# 若卡片尚未带反馈 footer（流式建卡时无 id），这里补挂
	if _items[_last_stream_idx].get_node_or_null("VBox/FeedbackRow") == null:
		_add_feedback_footer(_items[_last_stream_idx], message_id)
	_heights[_last_stream_idx] = _estimate_height(_items[_last_stream_idx])
	_request_sync()

func clear_messages() -> void:
	_unmount_all()
	_items.clear()
	_heights.clear()
	_mounted.clear()
	_last_stream_idx = -1
	_stream_text = ""
	_content.custom_minimum_size = Vector2.ZERO
	_request_sync()

## ---------------- 内部 ----------------

func _append_item(card: Control) -> void:
	_items.append(card)
	_heights.append(_estimate_height(card))
	_request_sync()

func _new_text_card(role: String, text: String) -> Control:
	var panel := PanelContainer.new()
	var label := RichTextLabel.new()
	label.bbcode_enabled = true
	label.fit_content = true
	label.scroll_active = false
	label.name = "Body"
	panel.add_child(label)
	_populate_text(panel, role, text)
	if role == "assistant" and panel.has_meta("message_id"):
		_add_feedback_footer(panel, str(panel.get_meta("message_id")))
	return panel

## 为 assistant 消息附加 like/dislike + note 控制条（message-feedback UI）。
## 按钮只发信号，RPC 由 main.gd 经 client.feedback_put 接线。
func _add_feedback_footer(panel: Control, message_id: String) -> void:
	var body: Control = panel.get_node_or_null("Body")
	if body == null:
		return
	var row := HBoxContainer.new()
	row.name = "FeedbackRow"
	row.custom_minimum_size = Vector2(0, FOOTER_H)
	row.add_theme_constant_override("separation", 6)
	var spacer := Control.new()
	spacer.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	row.add_child(spacer)

	var like := Button.new()
	like.text = "👍 Like"
	like.flat = true
	like.add_theme_font_size_override("font_size", 12)
	like.pressed.connect(func(): feedback_rating.emit(message_id, "like"))
	row.add_child(like)

	var dislike := Button.new()
	dislike.text = "👎 Dislike"
	dislike.flat = true
	dislike.add_theme_font_size_override("font_size", 12)
	dislike.pressed.connect(func(): feedback_rating.emit(message_id, "dislike"))
	row.add_child(dislike)

	var note_btn := Button.new()
	note_btn.text = "Add note"
	note_btn.flat = true
	note_btn.add_theme_font_size_override("font_size", 12)
	note_btn.pressed.connect(func(): _open_note_popup(panel, message_id))
	row.add_child(note_btn)

	# 将 Body 收进外层 VBox，Footer 追加其后
	var vbox := VBoxContainer.new()
	panel.remove_child(body)
	body.name = "Body"
	vbox.add_child(body)
	vbox.add_child(row)
	panel.add_child(vbox)

## note 弹层：输入反馈说明，确认后发 feedback_note 信号（经 feedback.put 的 note 字段）。
func _open_note_popup(panel: Control, message_id: String) -> void:
	var dialog := AcceptDialog.new()
	dialog.title = "Feedback note"
	dialog.ok_button_text = "Save"
	dialog.dialog_text = "Optional explanation:"
	var line := LineEdit.new()
	line.placeholder_text = "Why this rating? (optional)"
	line.custom_minimum_size = Vector2(320, 0)
	dialog.add_child(line)
	# AcceptDialog 内容区：包一个 MarginContainer 让输入框可见
	var vbox := VBoxContainer.new()
	vbox.add_child(line)
	# 替换默认 Message label 为提示 + 输入
	var msg := dialog.get_node_or_null("Message") as Label
	if msg != null:
		msg.text = "Attach a note to this message:"
	dialog.add_child(vbox)
	dialog.confirmed.connect(func():
		var note := line.text.strip_edges()
		if note != "":
			feedback_note.emit(message_id, note)
	)
	get_tree().current_scene.add_child(dialog)
	dialog.popup_centered(Vector2(380, 160))

func _populate_text(panel: Control, role: String, text: String) -> void:
	var label: RichTextLabel = panel.get_node_or_null("Body")
	if label == null:
		label = RichTextLabel.new()
		label.bbcode_enabled = true
		label.fit_content = true
		label.scroll_active = false
		label.name = "Body"
		panel.add_child(label)
	var escaped := text.replace("[", "[lb]").replace("]", "[/lb]")
	match role:
		"user":
			label.text = "[b][color=#4fc3f7]You:[/color][/b] " + escaped
		"assistant":
			label.text = _bb_assistant(escaped)
		"tool":
			label.text = "[color=#ffb74d][b]Tool:[/b][/color] " + escaped
		_:
			label.text = escaped

func _bb_assistant(t: String) -> String:
	return "[b][color=#81c784]DSH:[/color][/b]\n" + t

## 取卡片正文 RichTextLabel：旧版 Body 直接挂 panel，新版经 VBox 包裹。
func _find_body(card: Control) -> RichTextLabel:
	if card is PanelContainer and card.get_node_or_null("Body") is RichTextLabel:
		return card.get_node_or_null("Body") as RichTextLabel
	if card is PanelContainer and card.get_node_or_null("VBox/Body") is RichTextLabel:
		return card.get_node_or_null("VBox/Body") as RichTextLabel
	return null

func _estimate_height(card: Control) -> int:
	if card is PanelContainer:
		var lbl := _find_body(card)
		if lbl != null:
			var lines := 1
			if lbl.text != "":
				lines = maxi(lbl.text.count("\n") + 1, 1)
			return maxi(MIN_H, int(lines * CHAR_H + PAD) + FOOTER_H)
	return 0

func _request_sync() -> void:
	if _sync_pending:
		return
	_sync_pending = true
	call_deferred("_sync")

func _total() -> int:
	var t := 0
	for i in _heights.size():
		t += int(_heights[i])
	return t

func _unmount_all() -> void:
	for i in _mounted.size():
		var card: Control = _items[int(_mounted[i])]
		if is_instance_valid(card) and card.get_parent() == _content:
			_content.remove_child(card)
	_mounted.clear()

## ---------------- 虚拟裁剪布局 ----------------

func _sync() -> void:
	_sync_pending = false
	if not is_inside_tree():
		return
	var viewport_h := size.y if size.y > 0 else 480.0
	var offset := scroll_vertical
	var top := offset - OVERSCAN
	var bottom := offset + viewport_h + OVERSCAN

	# 计算可见窗口 [first, last)
	var first := -1
	var last := -1
	var cur_y := 0
	for i in _items.size():
		var h := int(_heights[i])
		if first < 0 and cur_y + h >= top:
			first = i
		if cur_y <= bottom:
			last = i + 1
		if cur_y >= bottom:
			break
		cur_y += h
	if first < 0:
		first = 0
	if last < 0:
		last = _items.size()
	if last > _items.size():
		last = _items.size()

	# 只增减在窗口外的项（差量挂载）
	# 卸载不再可见的
	var keep: Dictionary = {}
	for i in range(first, last):
		keep[i] = true
	var m := 0
	while m < _mounted.size():
		var idx := int(_mounted[m])
		if not keep.has(idx):
			var card: Control = _items[idx]
			if is_instance_valid(card) and card.get_parent() == _content:
				_content.remove_child(card)
			_mounted.remove_at(m)
		else:
			m += 1

	# 挂载窗口内、尚未挂载的
	var y_off := 0
	for i in range(first, last):
		if not _is_mounted(i):
			var card: Control = _items[i]
			_content.add_child(card)
			_mounted.append(i)
	# 重排所有已挂载项的位置（按新 first 重算 y）
	var acc := 0
	for i in range(first, last):
		acc += int(_heights[i])
		var card: Control = _items[i]
		card.position = Vector2(0, acc - int(_heights[i]))
		card.custom_minimum_size = Vector2(size.x, int(_heights[i]))
		card.visible = true
		# 挂载后实测校正
		var real := card.get_combined_minimum_size().y
		if real > 0:
			_heights[i] = int(real)

	_content.custom_minimum_size = Vector2(size.x, _total())

func _is_mounted(idx: int) -> bool:
	for m in _mounted:
		if int(m) == idx:
			return true
	return false
