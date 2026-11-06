"""JSON-lines transport for Frida's Python message API, never CLI display text."""
import base64
import hashlib
import json
import sys
import threading
from pathlib import Path
from datetime import datetime, timezone

import frida
import frida_tools


def run():
    config = json.loads(sys.stdin.readline())
    stop = threading.Event()
    output_lock = threading.Lock()
    state = {"failed": False, "closing": False}
    pid = 0
    session = None
    script = None
    resumed = False
    digest = hashlib.sha256(config["script"].encode("utf-8")).hexdigest()

    bridge_digest = ""

    def emit(kind, **fields):
        event = {"kind": kind, "package": config.get("package", ""),
                 "process": config.get("process", ""), "pid": pid,
                 "scriptSha256": digest, "runtimeVersion": frida.__version__,
                 "bridgeSha256": bridge_digest, **fields}
        with output_lock:
            print(json.dumps(event, ensure_ascii=True), flush=True)

    def fail(error):
        state["failed"] = True
        emit("collector_error", error=str(error))
        stop.set()

    def message(message, data):
        kind = message.get("type")
        payload = message.get("payload")
        if kind == "send":
            fields = {"payload": payload}
            if data is not None:
                fields["data"] = base64.b64encode(data).decode("ascii")
            emit("message", **fields)
        elif kind == "log":
            emit("log", log=str(payload), level=message.get("level", "info"))
        elif kind == "error":
            fail(message.get("stack", message.get("description", "script error")))
        else:
            fail("unknown Frida message type: " + str(kind))

    def detached(reason, crash=None):
        if not state["closing"]:
            fail("session detached: " + str(reason))

    def read_shutdown():
        sys.stdin.read()
        stop.set()

    def checkpoints():
        interval = config["checkpoint_interval_ms"] / 1000.0
        while not stop.wait(interval):
            requested = datetime.now(timezone.utc).isoformat()
            # The Python SDK's synchronous RPC has no deadline argument.
            # A bounded watchdog ends the session; detach unblocks the RPC.
            timer = threading.Timer(5.0, lambda: fail("checkpoint RPC timed out"))
            timer.daemon = True
            timer.start()
            try:
                result = script.exports_sync.craggycheckpoint()
                completed = datetime.now(timezone.utc).isoformat()
                if not stop.is_set():
                    emit("checkpoint", requestedAt=requested, completedAt=completed, payload=result)
            except Exception as error:
                if not state["closing"]:
                    fail("checkpoint RPC: " + str(error))
            finally:
                timer.cancel()

    threading.Thread(target=read_shutdown, daemon=True).start()
    device = frida.get_device_manager().add_remote_device(config["address"])
    try:
        # Frida 17's Python create_script has no implicit Java runtime.
        # Use the bridge installed with frida-tools; retain its exact identity.
        bridge_path = Path(frida_tools.__file__).parent / "bridges" / "java.js"
        bridge_source = bridge_path.read_text(encoding="utf-8")
        bridge_digest = hashlib.sha256(bridge_source.encode("utf-8")).hexdigest()
        source = "globalThis.Java = (function () {\n" + bridge_source + "\nreturn bridge;})();\n" + config["script"]
        if config.get("spawn"):
            pid = device.spawn(config["package"])
        elif config.get("process"):
            matches = [p for p in device.enumerate_processes()
                       if p.name == config["process"]]
            if len(matches) != 1:
                raise RuntimeError("target process name must identify exactly one running process")
            pid = matches[0].pid
        else:
            apps = device.enumerate_applications(identifiers=[config["package"]])
            if len(apps) != 1 or apps[0].pid == 0:
                raise RuntimeError("target package has no running process")
            pid = apps[0].pid
        session = device.attach(pid)
        session.on("detached", detached)
        emit("attached")
        script = session.create_script(source, name="cclab-observer", runtime="v8")
        script.set_log_handler(lambda level, text: emit("log", level=level, log=text))
        script.on("message", message)
        script.load()
        if state["failed"]:
            return 1
        if config.get("spawn"):
            device.resume(pid)
            resumed = True
        # Loaded is transport readiness. Individual hooks must separately
        # acknowledge installation; a successful load does not prove coverage.
        emit("loaded")
        if config.get("checkpoint_interval_ms", 0):
            threading.Thread(target=checkpoints, daemon=True).start()
        stop.wait()
        return 1 if state["failed"] else 0
    except Exception as error:
        fail(error)
        return 1
    finally:
        state["closing"] = True
        if session is not None:
            try:
                session.detach()
            except Exception:
                pass
        if config.get("spawn") and pid and not resumed:
            try:
                device.kill(pid)
            except Exception:
                pass


if __name__ == "__main__":
    sys.exit(run())
