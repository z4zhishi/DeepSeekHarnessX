extends ScrollContainer
class_name ChatView

const NODE_POOL_SIZE := 64

var message_container: VBoxContainer
var _pool: Array[Control] = []
var _active_nodes: Array[Control] = []
var _last_stream_card: PanelContainer = null
var _stream_buffer: String = ""

func _ready() -> void:
	message_container = VBoxContainer.new()
	message_container.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	message_container.add_theme_constant_override("separation", 12)
	add_child(message_container)

# 追加一条文本消息：从对象池取卡片（无则新建），渲染后挂到消息流。
func add_message(role: String, text: String) -> void:
	var card := _get_card()
	_populate(card, role, text)
	message_container.add_child(card)
	_active_nodes.append(card)

# 追加富内容卡片（diff / reasoning / terminal），不经过文本池。
func add_card(card: Control) -> void:
	message_container.add_child(card)
	_active_nodes.append(card)

# 流式追加：首次调用自动建卡，之后把增量文本 escape 后重建最后一卡。
func append_streaming(delta: String) -> void:
	if _last_stream_card == null:
		_last_stream_card = _get_card()
		_populate(_last_stream_card, "assistant", "")
		message_container.add_child(_last_stream_card)
		_active_nodes.append(_last_stream_card)
		_stream_buffer = ""
	_stream_buffer += delta
	var label := _last_stream_card.get_node("Body") as RichTextLabel
	label.text = "[b][color=#81c784]DSH:[/color][/b]\n" + _stream_buffer.replace("[", "[lb]").replace("]", "[/lb]")

# 结束流式（assistant/message 落库时调用），避免重复渲染。
func end_streaming() -> void:
	_last_stream_card = null
	_stream_buffer = ""

func clear_messages() -> void:
	_last_stream_card = null
	_stream_buffer = ""
	for node in _active_nodes:
		node.visible = false
		message_container.remove_child(node)
		# 只有带 Body 子节点的文本卡片回池复用；富内容卡片直接释放。
		if node is PanelContainer and node.get_node_or_null("Body") != null and _pool.size() < NODE_POOL_SIZE:
			_pool.append(node)
		else:
			node.queue_free()
	_active_nodes.clear()

func _get_card() -> PanelContainer:
	if not _pool.is_empty():
		return _pool.pop_back()
	return _new_card()

func _new_card() -> PanelContainer:
	var panel := PanelContainer.new()
	var label := RichTextLabel.new()
	label.bbcode_enabled = true
	label.fit_content = true
	label.name = "Body"
	panel.add_child(label)
	return panel

func _populate(panel: PanelContainer, role: String, text: String) -> void:
	var label := panel.get_node("Body") as RichTextLabel
	var escaped := text.replace("[", "[lb]").replace("]", "[/lb]")
	match role:
		"user":
			label.text = "[b][color=#4fc3f7]You:[/color][/b] " + escaped
		"assistant":
			label.text = "[b][color=#81c784]DSH:[/color][/b]\n" + escaped
		"tool":
			label.text = "[color=#ffb74d][b]Tool:[/b][/color] " + escaped
		_:
			label.text = escaped
	panel.visible = true