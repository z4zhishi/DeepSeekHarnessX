extends Node
class_name DshClient

signal session_event_received(event: Dictionary)
signal host_event_received(method: String, payload: Variant)
signal connection_state_changed(is_connected: bool)
signal connection_status_changed(status: String, attempt: int)
signal jobs_refreshed(jobs: Array)
signal mux_ready(session_id: String)

var base_url: String = "http://127.0.0.1:3080"
var ws_mux: WebSocketPeer = WebSocketPeer.new()
var ws_host: WebSocketPeer = WebSocketPeer.new()
var is_mux_connected := false
var is_host_connected := false
var active_session_id := ""
var _last_seq_by_session: Dictionary = {}
var _mux_session_id := ""
var _reconnect_timer: Timer = null
var _reconnect_attempts := 0
var _reconnect_scheduled := false
var _reported_connected := false
var _reported_status := ""

func _ready() -> void:
	var env_url := OS.get_environment("DSHX_GATEWAY_URL").strip_edges()
	if env_url != "": base_url = env_url.trim_suffix("/")
	_reconnect_timer = Timer.new()
	_reconnect_timer.one_shot = true
	_reconnect_timer.timeout.connect(_on_reconnect_timeout)
	add_child(_reconnect_timer)
	_connect_host()
	_connect_mux()

func _ws_origin() -> String:
	if base_url.begins_with("https://"): return "wss://" + base_url.substr(8)
	if base_url.begins_with("http://"): return "ws://" + base_url.substr(7)
	return base_url

func _emit_connection_status(status: String, attempt: int = 0) -> void:
	if status == _reported_status and attempt == _reconnect_attempts: return
	_reported_status = status
	connection_status_changed.emit(status, attempt)

func _update_connection_state() -> void:
	var connected := is_mux_connected and is_host_connected
	if connected != _reported_connected:
		_reported_connected = connected
		connection_state_changed.emit(connected)
	if connected: _emit_connection_status("connected", 0)

func _connect_mux() -> void:
	_emit_connection_status("connecting", _reconnect_attempts)
	_mux_session_id = active_session_id
	var from_seq := int(_last_seq_by_session.get(_mux_session_id, 0)) + 1
	var err := ws_mux.connect_to_url(_ws_origin() + "/api/events/mux?sessionId=" + _mux_session_id.uri_encode() + "&fromSeq=" + str(from_seq))
	if err != OK: _schedule_reconnect()

func _connect_host() -> void:
	_emit_connection_status("connecting", _reconnect_attempts)
	var err := ws_host.connect_to_url(_ws_origin() + "/api/events/host")
	if err != OK: _schedule_reconnect()

func connect_to_host() -> void:
	_connect_host(); _connect_mux()

func _on_reconnect_timeout() -> void:
	_reconnect_scheduled = false
	if not is_mux_connected and ws_mux.get_ready_state() == WebSocketPeer.STATE_CLOSED:
		ws_mux = WebSocketPeer.new(); _connect_mux()
	if not is_host_connected and ws_host.get_ready_state() == WebSocketPeer.STATE_CLOSED:
		ws_host = WebSocketPeer.new(); _connect_host()

func _schedule_reconnect() -> void:
	if _reconnect_scheduled or _reconnect_timer == null: return
	_reconnect_scheduled = true
	var delay := minf(30.0, 0.5 * pow(2.0, float(_reconnect_attempts)))
	_reconnect_attempts += 1
	_emit_connection_status("reconnecting", _reconnect_attempts)
	_reconnect_timer.start(delay)

func _process(_delta: float) -> void:
	_poll_socket(ws_mux, true); _poll_socket(ws_host, false)

func _poll_socket(socket: WebSocketPeer, is_mux: bool) -> void:
	socket.poll()
	var state := socket.get_ready_state()
	if state == WebSocketPeer.STATE_CONNECTING or state == WebSocketPeer.STATE_CLOSING: return
	if state == WebSocketPeer.STATE_OPEN:
		if is_mux:
			if _mux_session_id != active_session_id: return
			if not is_mux_connected:
				is_mux_connected = true; _update_connection_state(); mux_ready.emit(_mux_session_id)
		else:
			if not is_host_connected: is_host_connected = true; _update_connection_state()
		if is_mux_connected and is_host_connected:
			_reconnect_attempts = 0; _reconnect_scheduled = false
			if _reconnect_timer != null: _reconnect_timer.stop()
		var n := 0
		while socket.get_available_packet_count() > 0 and n < 48:
			n += 1; _handle_downlink_message(socket.get_packet().get_string_from_utf8(), is_mux)
		return
	if state != WebSocketPeer.STATE_CLOSED: return
	if is_mux:
		if is_mux_connected: is_mux_connected = false; _update_connection_state()
	else:
		if is_host_connected: is_host_connected = false; _update_connection_state()
	_schedule_reconnect()

func _handle_downlink_message(json_str: String, is_mux: bool) -> void:
	var json := JSON.new()
	if json.parse(json_str) != OK: return
	var data: Variant = json.get_data()
	if not data is Dictionary or str(data.get("type", "")) != "server-request": return
	var method := str(data.get("method", "")); var payload: Variant = data.get("payload", {})
	if is_mux:
		if method == "session/event" and payload is Dictionary:
			if _mux_session_id != active_session_id: return
			var seq := int(payload.get("seq", 0))
			if seq > 0:
				if seq <= int(_last_seq_by_session.get(_mux_session_id, 0)): return
				_last_seq_by_session[_mux_session_id] = seq
			session_event_received.emit(payload)
	else: host_event_received.emit(method, payload)

func _done(callback: Callable, ok: bool, value: Variant) -> void:
	if callback.is_valid(): callback.call(ok, value)

func _rpc(method: String, payload: Dictionary, callback: Callable) -> void:
	var req := HTTPRequest.new(); add_child(req)
	req.request_completed.connect(_on_rpc_completed.bind(req, callback))
	var err := req.request(base_url + "/api/" + method, PackedStringArray(["Content-Type: application/json"]), HTTPClient.METHOD_POST, JSON.stringify({"method": method, "payload": payload}))
	if err != OK: req.queue_free(); _done(callback, false, {"error": "request failed"})

func _on_rpc_completed(result: int, code: int, _headers: PackedStringArray, body: PackedByteArray, req: HTTPRequest, callback: Callable) -> void:
	if is_instance_valid(req): req.queue_free()
	var text := body.get_string_from_utf8(); var json := JSON.new()
	if result != HTTPRequest.RESULT_SUCCESS:
		_done(callback, false, _rpc_error_value(code, text, "request failed")); return
	if code != 200:
		_done(callback, false, _rpc_error_value(code, text, "HTTP %d" % code)); return
	if json.parse(text) != OK:
		_done(callback, false, {"error": "invalid response"}); return
	var parsed: Variant = json.get_data(); var res: Variant = parsed.get("result", {}) if parsed is Dictionary else {}
	if not res is Dictionary or not bool(res.get("ok", false)):
		_done(callback, false, {"error": _error_message(res.get("error", {}), "request failed") if res is Dictionary else "request failed"}); return
	_done(callback, true, res.get("value", {}))

func _rpc_error_value(code: int, body: String, fallback: String) -> Dictionary:
	var json := JSON.new()
	if json.parse(body) == OK:
		var parsed: Variant = json.get_data()
		if parsed is Dictionary:
			var err: Variant = (parsed as Dictionary).get("error", (parsed as Dictionary).get("result", {}))
			return {"error": _error_message(err, fallback), "status": code}
	return {"error": fallback, "status": code}

func _error_message(err: Variant, fallback: String) -> String:
	if err is Dictionary:
		var d: Dictionary = err
		for key in ["message", "error", "detail", "code"]:
			var value := str(d.get(key, "")).strip_edges()
			if value != "" and value != "<null>": return value
	var value := str(err).strip_edges()
	return value if value != "" and value != "<null>" else fallback

func set_active_session(session_id: String) -> void:
	active_session_id = session_id; is_mux_connected = false; _update_connection_state()
	if ws_mux.get_ready_state() != WebSocketPeer.STATE_CLOSED: ws_mux.close()
	ws_mux = WebSocketPeer.new(); _connect_mux()

func describe(callback: Callable) -> void: _rpc("host.describe", {}, callback)
func list_sessions(callback: Callable) -> void: _rpc("session.list", {}, callback)
func create_session(cwd: String, preset: String, callback: Callable) -> void: _rpc("session.create", {"cwd": cwd, "preset": preset}, callback)
func resume_session(id: String, callback: Callable) -> void: _rpc("session.resume", {"sessionId": id}, callback)
func fetch_history(id: String, from_seq: int, callback: Callable) -> void:
	_rpc("session.history", {"sessionId": id, "fromSeq": from_seq}, func(ok: bool, data: Variant) -> void:
		if ok:
			var events: Array = _history_events(data); var last := int(_last_seq_by_session.get(id, 0))
			for event in events:
				if event is Dictionary: last = maxi(last, int(event.get("seq", 0)))
			_last_seq_by_session[id] = last
		_done(callback, ok, data))
func _history_events(data: Variant) -> Array:
	if data is Array: return data
	if data is Dictionary and data.get("events") is Array: return data.get("events")
	return []
func send_prompt(id: String, text: String, callback: Callable) -> void: _rpc("session.prompt", {"sessionId": id, "text": text}, callback)
func steer_prompt(id: String, text: String, callback: Callable) -> void: _rpc("session.steer", {"sessionId": id, "text": text}, callback)
func abort_session(id: String, callback: Callable = Callable()) -> void: _rpc("session.abort", {"sessionId": id}, callback)
func stop_session(id: String, callback: Callable = Callable()) -> void: _rpc("session.stop", {"sessionId": id}, callback)
func send_command(id: String, line: String, callback: Callable) -> void: _rpc("session.command", {"sessionId": id, "line": line}, callback)
func list_commands(callback: Callable) -> void: _rpc("command.list", {}, callback)
func respond_approval(call_id: String, decision: String, callback: Callable = Callable()) -> void: _rpc("approval.respond", {"callId": call_id, "decision": decision}, callback)
func list_jobs(id: String, callback: Callable = Callable()) -> void: _rpc("jobs.list", {"sessionId": id}, func(ok: bool, data: Variant) -> void:
	var jobs: Array = []
	if ok and data is Dictionary and data.get("jobs") is Array: jobs = data["jobs"]
	jobs_refreshed.emit(jobs); _done(callback, ok, data))
func read_job_output(id: String, job: String, callback: Callable) -> void: _rpc("jobs.output", {"sessionId": id, "jobId": job}, callback)
func kill_job(id: String, job: String, callback: Callable) -> void: _rpc("jobs.kill", {"sessionId": id, "jobId": job}, callback)
func list_workspaces(callback: Callable) -> void: _rpc("workspace.list", {}, callback)
func settings_describe(callback: Callable) -> void: _rpc("settings.describe", {}, callback)
func settings_mutate(ns: String, ops: Array, callback: Callable) -> void: _rpc("settings.mutate", {"ns": ns, "ops": _normalize_ops(ops)}, callback)
func _normalize_ops(ops: Array) -> Array:
	var out: Array = []
	for op in ops:
		if not op is Dictionary: continue
		var d: Dictionary = (op as Dictionary).duplicate(); var path: Variant = d.get("path", d.get("Path", []))
		if path is String: d["path"] = Array(str(path).split(".", false))
		elif path is PackedStringArray: d["path"] = Array(path)
		out.append(d)
	return out
func list_models(callback: Callable) -> void: _rpc("llm.models", {}, callback)
func set_model(model: String, callback: Callable = Callable()) -> void: _rpc("model.set", {"model": model}, callback)
func provider_describe(callback: Callable = Callable()) -> void: _rpc("provider.describe", {}, callback)
func provider_set(payload: Dictionary, callback: Callable = Callable()) -> void: _rpc("provider.set", payload, callback)
func provider_apply(callback: Callable = Callable()) -> void: _rpc("provider.apply", {}, callback)
func provider_delete(id: String, callback: Callable = Callable()) -> void: _rpc("provider.delete", {"id": id}, callback)
func provider_models(id: String = "", callback: Callable = Callable()) -> void: _rpc("provider.models", {"profileId": id} if id != "" else {}, callback)
func settings_credentials_describe(ref: String, callback: Callable = Callable()) -> void: _rpc("settings.credentials.describe", {"ref": ref}, callback)
func context_limit_get(callback: Callable = Callable()) -> void: _rpc("context.limit.get", {}, callback)
func context_limit_set(limit_k: float = 0.0, reset: bool = false, callback: Callable = Callable()) -> void: _rpc("context.limit.set", {"reset": reset, "limitK": limit_k}, callback)
func session_context(id: String, callback: Callable) -> void: _rpc("session.context", {"sessionId": id}, callback)
func session_policy(id: String, callback: Callable) -> void: _rpc("session.policy", {"sessionId": id}, callback)
func session_effort(id: String, effort: String, callback: Callable = Callable()) -> void: _rpc("session.effort", {"sessionId": id, "effort": effort}, callback)
func feedback_put(id: String, message: String, rating: String, note: String = "", version: String = "", callback: Callable = Callable()) -> void: _rpc("feedback.put", {"sessionId": id, "messageId": message, "rating": rating, "note": note, "version": version}, callback)
func plugin_list(callback: Callable) -> void: _rpc("plugin.list", {}, callback)
func plugin_install(path: String, callback: Callable = Callable()) -> void: _rpc("plugin.install", {"path": path}, callback)
func plugin_uninstall(name: String, callback: Callable = Callable()) -> void: _rpc("plugin.uninstall", {"name": name}, callback)
func plugin_enable(name: String, enabled: bool, callback: Callable = Callable()) -> void: _rpc("plugin.enable", {"name": name, "enabled": enabled}, callback)
func session_rename(id: String, title: String, callback: Callable = Callable()) -> void: _rpc("session.rename", {"sessionId": id, "title": title}, callback)
func session_delete(id: String, callback: Callable = Callable()) -> void: _rpc("session.delete", {"sessionId": id}, callback)
