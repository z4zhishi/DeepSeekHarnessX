extends Node
class_name DshClient

## HTTP RPC + dual WebSocket downlinks for the Go gateway.
## POST /api/<method> body {method, payload}; WS mux + host are independent.

signal session_event_received(event: Dictionary)
signal host_event_received(method: String, payload: Variant)
signal connection_state_changed(is_connected: bool)
signal jobs_refreshed(jobs: Array)
signal mux_ready(session_id: String)

var base_url: String = "http://127.0.0.1:3080"
var ws_mux: WebSocketPeer = WebSocketPeer.new()
var ws_host: WebSocketPeer = WebSocketPeer.new()

var is_mux_connected: bool = false
var is_host_connected: bool = false
var active_session_id: String = ""

var _reconnect_timer: Timer = null
var _reconnect_attempts: int = 0
var _reconnect_scheduled: bool = false
var _mux_alive: bool = false
var _host_alive: bool = false


func _ready() -> void:
	# Gateway endpoint injection: the Go host (embedgui) sets DSHX_GATEWAY_URL
	# when launching this window, so --host/--port take effect. Empty env keeps
	# the default (standalone/dev runs against a 3080 gateway).
	var env_url := OS.get_environment("DSHX_GATEWAY_URL").strip_edges()
	if env_url != "":
		base_url = env_url.trim_suffix("/")
	_reconnect_timer = Timer.new()
	_reconnect_timer.one_shot = true
	_reconnect_timer.timeout.connect(_on_reconnect_timeout)
	add_child(_reconnect_timer)
	if ws_host.get_ready_state() == WebSocketPeer.STATE_CLOSED:
		_connect_host()
	if ws_mux.get_ready_state() == WebSocketPeer.STATE_CLOSED:
		_connect_mux()


func _ws_origin() -> String:
	if base_url.begins_with("https://"):
		return "wss://" + base_url.substr("https://".length())
	if base_url.begins_with("http://"):
		return "ws://" + base_url.substr("http://".length())
	return base_url


func _connect_mux() -> void:
	var url := _ws_origin() + "/api/events/mux?sessionId=" + active_session_id.uri_encode()
	ws_mux.connect_to_url(url)


func _connect_host() -> void:
	var url := _ws_origin() + "/api/events/host"
	ws_host.connect_to_url(url)


func connect_to_host() -> void:
	_connect_host()
	_connect_mux()


func _on_reconnect_timeout() -> void:
	_reconnect_scheduled = false
	if not is_mux_connected and ws_mux.get_ready_state() == WebSocketPeer.STATE_CLOSED:
		ws_mux = WebSocketPeer.new()
		_mux_alive = false
		_connect_mux()
	if not is_host_connected and ws_host.get_ready_state() == WebSocketPeer.STATE_CLOSED:
		ws_host = WebSocketPeer.new()
		_host_alive = false
		_connect_host()


func _schedule_reconnect() -> void:
	if _reconnect_scheduled:
		return
	if _reconnect_timer == null:
		return
	_reconnect_scheduled = true
	var delay := minf(30.0, 0.5 * pow(2.0, float(_reconnect_attempts)))
	_reconnect_attempts += 1
	_reconnect_timer.start(delay)


func _process(_delta: float) -> void:
	_poll_socket(ws_mux, true)
	_poll_socket(ws_host, false)


func _poll_socket(socket: WebSocketPeer, is_mux: bool) -> void:
	socket.poll()
	var state := socket.get_ready_state()
	if state == WebSocketPeer.STATE_CONNECTING or state == WebSocketPeer.STATE_CLOSING:
		if is_mux:
			_mux_alive = true
		else:
			_host_alive = true
		return
	if state == WebSocketPeer.STATE_OPEN:
		if is_mux:
			_mux_alive = true
			if not is_mux_connected:
				is_mux_connected = true
				connection_state_changed.emit(true)
				mux_ready.emit(active_session_id)
		else:
			_host_alive = true
			if not is_host_connected:
				is_host_connected = true
		if is_mux_connected and is_host_connected:
			_reconnect_attempts = 0
			_reconnect_scheduled = false
			if _reconnect_timer != null:
				_reconnect_timer.stop()
		while socket.get_available_packet_count() > 0:
			var packet := socket.get_packet()
			_handle_downlink_message(packet.get_string_from_utf8(), is_mux)
		return
	if state != WebSocketPeer.STATE_CLOSED:
		return
	if is_mux:
		var drop := _mux_alive
		_mux_alive = false
		if is_mux_connected:
			is_mux_connected = false
			connection_state_changed.emit(false)
		if drop:
			_schedule_reconnect()
	else:
		var drop := _host_alive
		_host_alive = false
		if is_host_connected:
			is_host_connected = false
		if drop:
			_schedule_reconnect()


func _handle_downlink_message(json_str: String, is_mux: bool) -> void:
	var json := JSON.new()
	if json.parse(json_str) != OK:
		return
	var data: Variant = json.get_data()
	if not (data is Dictionary):
		return
	var dict: Dictionary = data
	if str(dict.get("type", "")) != "server-request":
		return
	var method := str(dict.get("method", ""))
	var payload: Variant = dict.get("payload", {})
	if is_mux:
		if method == "session/event" and payload is Dictionary:
			session_event_received.emit(payload)
		return
	host_event_received.emit(method, payload)


func _done(callback: Callable, ok: bool, value: Variant) -> void:
	if callback.is_valid():
		callback.call(ok, value)


func _rpc(method: String, payload: Dictionary, callback: Callable) -> void:
	var url := base_url + "/api/" + method
	var headers := PackedStringArray(["Content-Type: application/json"])
	var body := JSON.stringify({"method": method, "payload": payload})
	var req := HTTPRequest.new()
	add_child(req)
	req.request_completed.connect(_on_rpc_completed.bind(req, callback))
	var err := req.request(url, headers, HTTPClient.METHOD_POST, body)
	if err != OK:
		req.queue_free()
		_done(callback, false, {"error": "request failed"})


func _on_rpc_completed(result: int, code: int, _headers: PackedStringArray, resp_body: PackedByteArray, req: HTTPRequest, callback: Callable) -> void:
	if is_instance_valid(req):
		req.queue_free()
	var body := resp_body.get_string_from_utf8()
	if result != HTTPRequest.RESULT_SUCCESS:
		_done(callback, false, _rpc_error_value(code, body, "request failed"))
		return
	var json := JSON.new()
	var parsed: Variant = {}
	var parsed_ok := json.parse(body) == OK
	if parsed_ok:
		parsed = json.get_data()
	if code != 200:
		_done(callback, false, _rpc_error_value(code, body, "HTTP %d" % code))
		return
	if not parsed_ok:
		_done(callback, false, {"error": "invalid response"})
		return
	if not (parsed is Dictionary):
		_done(callback, false, {"error": "invalid response"})
		return
	var res: Variant = (parsed as Dictionary).get("result", {})
	if not (res is Dictionary):
		_done(callback, false, {"error": "invalid response"})
		return
	var result_dict: Dictionary = res
	if not bool(result_dict.get("ok", false)):
		_done(callback, false, {"error": _error_message(result_dict.get("error", {}), "request failed")})
		return
	_done(callback, true, result_dict.get("value", {}))


func _rpc_error_value(code: int, body: String, fallback: String) -> Dictionary:
	var json := JSON.new()
	if json.parse(body) == OK:
		var parsed: Variant = json.get_data()
		if parsed is Dictionary:
			var d: Dictionary = parsed
			var res: Variant = d.get("result", d)
			if res is Dictionary:
				var msg := _error_message((res as Dictionary).get("error", d.get("error", {})), "")
				if msg != "":
					return {"error": msg}
			var top := _error_message(d.get("error", d.get("message", "")), "")
			if top != "":
				return {"error": top}
	if code > 0:
		return {"error": "HTTP %d" % code}
	return {"error": fallback}


func _error_message(err: Variant, fallback: String) -> String:
	if err is Dictionary:
		var msg := str((err as Dictionary).get("message", (err as Dictionary).get("error", "")))
		return msg if msg != "" else fallback
	var s := str(err).strip_edges()
	if s == "" or s == "<null>":
		return fallback
	return s


func set_active_session(session_id: String) -> void:
	active_session_id = session_id
	is_mux_connected = false
	_mux_alive = false
	if ws_mux.get_ready_state() != WebSocketPeer.STATE_CLOSED:
		ws_mux.close()
	ws_mux = WebSocketPeer.new()
	_connect_mux()


func describe(callback: Callable) -> void:
	_rpc("host.describe", {}, callback)


func list_sessions(callback: Callable) -> void:
	_rpc("session.list", {}, callback)


func create_session(cwd: String, preset: String, callback: Callable) -> void:
	_rpc("session.create", {"cwd": cwd, "preset": preset}, callback)


func resume_session(id: String, callback: Callable) -> void:
	_rpc("session.resume", {"sessionId": id}, callback)


func fetch_history(id: String, from_seq: int, callback: Callable) -> void:
	_rpc("session.history", {"sessionId": id, "fromSeq": from_seq}, callback)


func send_prompt(id: String, text: String, callback: Callable) -> void:
	_rpc("session.prompt", {"sessionId": id, "text": text}, callback)


func steer_prompt(id: String, text: String, callback: Callable) -> void:
	_rpc("session.steer", {"sessionId": id, "text": text}, callback)


func abort_session(id: String, callback: Callable = Callable()) -> void:
	_rpc("session.abort", {"sessionId": id}, callback)


func stop_session(id: String, callback: Callable = Callable()) -> void:
	_rpc("session.stop", {"sessionId": id}, callback)


func send_command(id: String, line: String, callback: Callable) -> void:
	_rpc("session.command", {"sessionId": id, "line": line}, callback)


func list_commands(callback: Callable) -> void:
	_rpc("command.list", {}, callback)


func respond_approval(call_id: String, decision: String, callback: Callable = Callable()) -> void:
	_rpc("approval.respond", {"callId": call_id, "decision": decision}, callback)


func list_jobs(session_id: String, callback: Callable = Callable()) -> void:
	_rpc("jobs.list", {"sessionId": session_id}, _on_jobs_listed.bind(callback))


func _on_jobs_listed(ok: bool, data: Variant, callback: Callable) -> void:
	var jobs: Array = []
	if ok and data is Dictionary and (data as Dictionary).get("jobs") is Array:
		jobs = (data as Dictionary)["jobs"] as Array
	jobs_refreshed.emit(jobs)
	_done(callback, ok, data)


func read_job_output(session_id: String, job_id: String, callback: Callable) -> void:
	_rpc("jobs.output", {"sessionId": session_id, "jobId": job_id}, callback)


func kill_job(session_id: String, job_id: String, callback: Callable) -> void:
	_rpc("jobs.kill", {"sessionId": session_id, "jobId": job_id}, callback)


func list_workspaces(callback: Callable) -> void:
	_rpc("workspace.list", {}, callback)


## Full settings mirror read; consumers derive per-namespace values from
## `namespaces[].user` / `base` (see app.gd `_restore_language`).
func settings_describe(callback: Callable) -> void:
	_rpc("settings.describe", {}, callback)


func settings_mutate(ns: String, ops: Array, callback: Callable) -> void:
	_rpc("settings.mutate", {"ns": ns, "ops": _normalize_ops(ops)}, callback)


func _normalize_ops(ops: Array) -> Array:
	var out: Array = []
	for item in ops:
		if not (item is Dictionary):
			continue
		var op: Dictionary = (item as Dictionary).duplicate()
		var path: Variant = op.get("path", [])
		if path is String:
			var s := str(path)
			if s.contains("."):
				op["path"] = Array(s.split("."))
			else:
				op["path"] = [s]
		elif path is PackedStringArray:
			var arr: Array = []
			for p in path:
				arr.append(String(p))
			op["path"] = arr
		elif not (path is Array):
			op["path"] = [str(path)]
		out.append(op)
	return out


func list_models(callback: Callable) -> void:
	_rpc("llm.models", {}, callback)


func set_model(model: String, callback: Callable = Callable()) -> void:
	_rpc("model.set", {"model": model}, callback)


func provider_describe(callback: Callable = Callable()) -> void:
	_rpc("provider.describe", {}, callback)


func provider_set(payload: Dictionary, callback: Callable = Callable()) -> void:
	_rpc("provider.set", payload, callback)


func provider_apply(callback: Callable = Callable()) -> void:
	_rpc("provider.apply", {}, callback)


func provider_delete(id: String, callback: Callable = Callable()) -> void:
	_rpc("provider.delete", {"id": id}, callback)


func provider_models(profile_id: String = "", callback: Callable = Callable()) -> void:
	var payload := {}
	if profile_id != "":
		payload["profileId"] = profile_id
	_rpc("provider.models", payload, callback)


func settings_credentials_describe(ref: String, callback: Callable = Callable()) -> void:
	_rpc("settings.credentials.describe", {"ref": ref}, callback)


func session_context(session_id: String, callback: Callable) -> void:
	_rpc("session.context", {"sessionId": session_id}, callback)


func feedback_put(session_id: String, message_id: String, rating: String, note: String = "", version: String = "", callback: Callable = Callable()) -> void:
	var payload := {"sessionId": session_id, "messageId": message_id, "rating": rating}
	if note != "":
		payload["note"] = note
	if version != "":
		payload["version"] = version
	_rpc("feedback.put", payload, callback)


func plugin_list(callback: Callable) -> void:
	_rpc("plugin.list", {}, callback)


func plugin_install(path: String, callback: Callable = Callable()) -> void:
	_rpc("plugin.install", {"path": path}, callback)


func plugin_uninstall(name: String, callback: Callable = Callable()) -> void:
	_rpc("plugin.uninstall", {"name": name}, callback)


func plugin_enable(name: String, enabled: bool, callback: Callable = Callable()) -> void:
	_rpc("plugin.enable", {"name": name, "enabled": enabled}, callback)
