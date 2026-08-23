extends Node
class_name DshClient

signal session_event_received(event_data: Dictionary)
signal host_event_received(method: String, payload: Variant)
signal connection_state_changed(is_connected: bool)
signal jobs_refreshed(jobs: Array)

var base_url: String = "http://127.0.0.1:3080"
var ws_mux: WebSocketPeer = WebSocketPeer.new()
var ws_host: WebSocketPeer = WebSocketPeer.new()
var http_client: HTTPRequest

var is_mux_connected: bool = false
var is_host_connected: bool = false
var active_session_id: String = ""

func _ready() -> void:
	http_client = HTTPRequest.new()
	add_child(http_client)
	connect_to_host()

func connect_to_host() -> void:
	var mux_url = base_url.replace("http://", "ws://") + "/api/events/mux"
	if active_session_id != "":
		mux_url += "?sessionId=" + active_session_id
	var host_url = base_url.replace("http://", "ws://") + "/api/events/host"
	
	ws_mux.connect_to_url(mux_url)
	ws_host.connect_to_url(host_url)

func _process(_delta: float) -> void:
	_poll_socket(ws_mux, true)
	_poll_socket(ws_host, false)

func _poll_socket(socket: WebSocketPeer, is_mux: bool) -> void:
	socket.poll()
	var state = socket.get_ready_state()
	
	if state == WebSocketPeer.STATE_OPEN:
		if is_mux and not is_mux_connected:
			is_mux_connected = true
			connection_state_changed.emit(true)
		elif not is_mux and not is_host_connected:
			is_host_connected = true
			
		while socket.get_available_packet_count() > 0:
			var packet = socket.get_packet()
			var text = packet.get_string_from_utf8()
			_handle_downlink_message(text, is_mux)
			
	elif state == WebSocketPeer.STATE_CLOSED:
		if is_mux and is_mux_connected:
			is_mux_connected = false
			connection_state_changed.emit(false)
		elif not is_mux and is_host_connected:
			is_host_connected = false

func _handle_downlink_message(json_str: String, is_mux: bool) -> void:
	var json = JSON.new()
	if json.parse(json_str) != OK:
		return
		
	var dict = json.get_data() as Dictionary
	if not dict.has("type") or dict["type"] != "server-request":
		return
		
	var method = dict.get("method", "")
	var payload = dict.get("payload", {})
	
	if is_mux:
		if method == "session/event":
			session_event_received.emit(payload)
	else:
		host_event_received.emit(method, payload)

# 统一 RPC：POST /api/<method>，body {method, payload}，回调 (ok, data)
func _rpc(method: String, payload: Dictionary, callback: Callable) -> void:
	var url = base_url + "/api/" + method
	var headers = ["Content-Type: application/json"]
	var body = JSON.stringify({
		"method": method,
		"payload": payload
	})
	var req = HTTPRequest.new()
	add_child(req)
	req.request_completed.connect(func(result, code, _headers, resp_body):
		req.queue_free()
		if code == 200:
			var json = JSON.new()
			if json.parse(resp_body.get_string_from_utf8()) == OK:
				var resp = json.get_data() as Dictionary
				var res = resp.get("result", {})
				callback.call(res.get("ok", false), res.get("value", {}))
				return
		callback.call(false, {})
	)
	req.request(url, headers, HTTPClient.METHOD_POST, body)

# 切换活动会话：重连 mux 下行（带 sessionId 过滤），host 通道保持。
func set_active_session(session_id: String) -> void:
	active_session_id = session_id
	is_mux_connected = false
	ws_mux.close()
	connect_to_host()

func describe(callback: Callable) -> void:
	_rpc("host.describe", {}, callback)

func list_sessions(callback: Callable) -> void:
	_rpc("session.list", {}, callback)

func fetch_history(session_id: String, from_seq: int, callback: Callable) -> void:
	_rpc("session.history", {"sessionId": session_id, "fromSeq": from_seq}, callback)

func respond_approval(call_id: String, decision: String, callback: Callable = Callable()) -> void:
	_rpc("approval.respond", {"callId": call_id, "decision": decision}, callback)

func send_prompt(session_id: String, text: String, callback: Callable) -> void:
	_rpc("session.prompt", {"sessionId": session_id, "text": text}, callback)

func send_command(session_id: String, line: String, callback: Callable) -> void:
	_rpc("session.command", {"sessionId": session_id, "line": line}, callback)

func create_session(cwd: String, preset: String, callback: Callable) -> void:
	_rpc("session.create", {"cwd": cwd, "preset": preset}, callback)

## ---- jobs RPC（对应 gateway jobs.list / jobs.output / jobs.kill）----
## 供 jobs 面板 / 右侧详情栏（波3）消费；返回 (ok, data) 回调。

# jobs.list  -> { "jobs": [JobPublic{id,kind,label,status,detail,startedAt,finishedAt}] }
func list_jobs(session_id: String, callback: Callable) -> void:
	_rpc("jobs.list", {"sessionId": session_id}, func(ok, data):
		var jobs: Array = []
		if ok and data is Dictionary and data.get("jobs") is Array:
			jobs = data["jobs"] as Array
		jobs_refreshed.emit(jobs)
		callback.call(ok, data)
	)

# jobs.output -> { "output": string }
func read_job_output(session_id: String, job_id: String, callback: Callable) -> void:
	_rpc("jobs.output", {"sessionId": session_id, "jobId": job_id}, callback)

# jobs.kill   -> { "killed": jobId }
func kill_job(session_id: String, job_id: String, callback: Callable) -> void:
	_rpc("jobs.kill", {"sessionId": session_id, "jobId": job_id}, callback)

## ---- workspace / settings（占位接口，波3 接线）----

# workspace.list -> [ {id, name, path} ]
func list_workspaces(callback: Callable) -> void:
	_rpc("workspace.list", {}, callback)

# settings 面板骨架：后端暂无独立 settings RPC，先经 host.describe 取运行时信息。
func fetch_settings(callback: Callable) -> void:
	describe(callback)