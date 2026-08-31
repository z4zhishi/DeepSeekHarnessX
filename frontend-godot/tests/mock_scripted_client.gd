extends "res://scripts/net/dsh_client.gd"

## tests/mock_scripted_client.gd — GUI 探针专用假网关（仅测试文件，非产品路径）。
##
## Seam 契约（与 dsh_client.gd 的真实形状一一对应，不发明平行后端路径）：
##   上行：所有 session.* / host.* RPC 都汇于基类 `_rpc()`（HTTPRequest 出口），
##         本 mock 只覆写这一个方法，改为查表回调；
##   下行：真实下行解析入口是基类 `_handle_downlink_message(json, is_mux)`
##         （WS 帧 → JSON 解析 → seq 去重 → session_event_received /
##         host_event_received 信号），探针投递事件仍走这同一条
##         服务器帧解析路径，只是把"网络送达"换成"脚本注入"；
##   连接：`set_active_session` 在真实实现里重连 mux WS 并在就绪后发
##         mux_ready；探针的等价物是置位两个 is_*_connected 并同步发
##         mux_ready（与 _poll_socket 成功分支相同的信号序列），且不发起
##         任何真实网络连接。

## 记录每一次 RPC：[{method, payload}]，供探针断言调用序（approval.respond
## 的 decision、session.prompt 的文本等）。
var rpc_log: Array = []

## method -> Callable(payload: Dictionary, callback: Callable)
var _scripted: Dictionary = {}
## 未脚本化的 method 走这里（null = 默认成功空载荷）
var _default_ok := true
## set_active_session 推迟广播的会话 id（真实 mux_ready 异步时序）
var _mux_ready_target := ""


func script_response(method: String, handler: Callable) -> void:
	_scripted[method] = handler


func rpc_calls(method: String) -> Array:
	var out: Array = []
	for entry in rpc_log:
		if entry is Dictionary and str(entry.get("method", "")) == method:
			out.append(entry)
	return out


func _ready() -> void:
	# 窗口/UI 探针下绝不发起真实 WS/HTTP（父 _ready 会连接 /api/events/*）。
	pass


func _process(_delta: float) -> void:
	pass


func _rpc(method: String, payload: Dictionary, callback: Callable) -> void:
	rpc_log.append({"method": method, "payload": payload.duplicate(true)})
	var handler: Variant = _scripted.get(method)
	if handler is Callable:
		(handler as Callable).call(payload, callback)
		return
	if not _default_ok:
		_done(callback, false, {"error": "unhandled method %s" % method})
		return
	_done(callback, true, {})


func set_active_session(session_id: String) -> void:
	# 真实等价物：WS mux/host 握手完成的信号时序。真实实现里 mux_ready 只会
	# 在 WS 就绪后（下一帧 _poll_socket）异步到达，晚于 app._switch 的同步
	# 清屏/复位——这里同样把 mux_ready 推迟到下一帧，保持「先切后载」次序。
	active_session_id = session_id
	_mux_session_id = session_id
	is_mux_connected = true
	is_host_connected = true
	_update_connection_state()
	_mux_ready_target = session_id
	call_deferred("_emit_mux_ready")


func _emit_mux_ready() -> void:
	mux_ready.emit(_mux_ready_target)


## 经真实下行解析路径投递一条 mux 会话事件（等价于服务端 WS 帧）。
func deliver_session_event(event: Dictionary) -> void:
	if _mux_session_id == "":
		_mux_session_id = active_session_id
	_handle_downlink_message(
		JSON.stringify({"type": "server-request", "method": "session/event", "payload": event}),
		true)


## 经真实下行解析路径投递一条 host 事件（等价于 host WS 帧的 server-request）。
func deliver_host_event(method: String, payload: Variant) -> void:
	_handle_downlink_message(
		JSON.stringify({"type": "server-request", "method": method, "payload": payload}),
		false)