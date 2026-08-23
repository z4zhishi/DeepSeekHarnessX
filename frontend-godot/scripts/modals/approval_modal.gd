# 审批权限弹框：宿主下行 host/permission-request 时弹出。
# 选项来自后端 options 数组（allow_once/deny/cancel，或任意问答选项）。
# 选项按钮通过 AcceptDialog 的 add_button 进入底部按钮槽（button slot），
# 而非 add_child 进内容区。点击后经 decision_made 信号送出，由 main.gd
# 通过 approval.respond RPC 回填。

extends AcceptDialog
class_name ApprovalModal

signal decision_made(call_id: String, decision: String)

var _pending_call_id: String = ""
var _option_buttons: Array[Button] = []

func _ready() -> void:
	confirmed.connect(_on_request_allow)
	canceled.connect(_on_cancel)

func show_request(call_id: String, prompt: String, options: Array = []) -> void:
	_pending_call_id = call_id
	var msg := get_node_or_null("Message") as Label
	if msg != null:
		msg.text = prompt
	_clear_option_buttons()
	if options.is_empty():
		# 兜底：与后端固定三选项一致的默认集合
		options = [
			{"optionId": "allow_once", "name": "Allow once"},
			{"optionId": "deny", "name": "Reject"},
			{"optionId": "cancel", "name": "Cancel"},
		]
	for opt in options:
		if opt is Dictionary:
			var opt_id: String = str(opt.get("optionId", ""))
			var label: String = str(opt.get("name", opt_id if opt_id != "" else "?"))
			# add_button 把按钮放进 AcceptDialog 底部按钮槽（button slot）。
			var btn := add_button(label)
			btn.pressed.connect(_on_option.bind(opt_id))
			_option_buttons.append(btn)

func _on_option(option_id: String) -> void:
	_decide(option_id)

func _on_request_allow() -> void:
	_decide("allow_once")

func _clear_option_buttons() -> void:
	for btn in _option_buttons:
		remove_button(btn)
		btn.queue_free()
	_option_buttons.clear()

func _on_cancel() -> void:
	_decide("cancel")

func _decide(decision: String) -> void:
	if _pending_call_id == "":
		return
	var call_id := _pending_call_id
	_pending_call_id = ""
	hide()
	decision_made.emit(call_id, decision)
