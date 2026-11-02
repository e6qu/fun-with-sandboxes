"""The Python runtime of the fws Lambda images: a client of the Lambda
Runtime API that loads the handler and runs each invocation, as AWS's
runtime interface client does. The handler is module.function, the module
found under LAMBDA_TASK_ROOT, and is called with the event and a context."""

import importlib
import json
import os
import sys
import time
import traceback
import urllib.error
import urllib.request

API = "http://" + os.environ["AWS_LAMBDA_RUNTIME_API"] + "/2018-06-01/runtime"


def host_path(p):
    """Map a path inside the container to the host, where SANDBOX_ROOT
    holds the container's root."""
    root = os.environ.get("SANDBOX_ROOT", "")
    if root and not p.startswith(root + "/"):
        return root + p
    return p


def request(method, url, body=None, headers=None):
    req = urllib.request.Request(url, data=body, method=method, headers=headers or {})
    return urllib.request.urlopen(req, timeout=None)


def post(path, payload, headers=None):
    data = json.dumps(payload).encode()
    h = {"Content-Type": "application/json"}
    h.update(headers or {})
    with request("POST", API + path, data, h) as res:
        res.read()


def error_payload(exc):
    return {
        "errorMessage": str(exc),
        "errorType": type(exc).__name__,
        "requestId": "",
        # The runtime's own frames are not the function's.
        "stackTrace": traceback.format_list(
            [f for f in traceback.extract_tb(exc.__traceback__) if f.filename != __file__]
        ),
    }


class LambdaContext:
    def __init__(self, headers):
        self.aws_request_id = headers.get("Lambda-Runtime-Aws-Request-Id")
        self._deadline_ms = int(headers.get("Lambda-Runtime-Deadline-Ms", "0"))
        self.invoked_function_arn = headers.get("Lambda-Runtime-Invoked-Function-Arn")
        self.function_name = os.environ.get("AWS_LAMBDA_FUNCTION_NAME")
        self.function_version = os.environ.get("AWS_LAMBDA_FUNCTION_VERSION")
        self.memory_limit_in_mb = os.environ.get("AWS_LAMBDA_FUNCTION_MEMORY_SIZE")
        self.log_group_name = os.environ.get("AWS_LAMBDA_LOG_GROUP_NAME")
        self.log_stream_name = os.environ.get("AWS_LAMBDA_LOG_STREAM_NAME")
        cognito = headers.get("Lambda-Runtime-Cognito-Identity")
        self.identity = json.loads(cognito) if cognito else None
        client = headers.get("Lambda-Runtime-Client-Context")
        self.client_context = json.loads(client) if client else None

    def get_remaining_time_in_millis(self):
        return max(self._deadline_ms - int(time.time() * 1000), 0)


def load_handler(spec):
    module_path, _, function = spec.rpartition(".")
    if not module_path or not function:
        raise ValueError("Bad handler '%s': not enough values to unpack (expected 2, got 1)" % spec)
    sys.path.insert(0, host_path(os.environ.get("LAMBDA_TASK_ROOT", "/var/task")))
    module = importlib.import_module(module_path.replace("/", "."))
    handler = getattr(module, function, None)
    if handler is None:
        raise AttributeError("Handler '%s' missing on module '%s'" % (function, module_path))
    return handler


def main():
    spec = sys.argv[-1] if len(sys.argv) > 1 else os.environ.get("_HANDLER", "")
    try:
        handler = load_handler(spec)
    except Exception as exc:
        traceback.print_exc()
        post("/init/error", error_payload(exc), {"Lambda-Runtime-Function-Error-Type": "Runtime.ImportModuleError"})
        sys.exit(1)
    while True:
        with request("GET", API + "/invocation/next") as res:
            headers = dict(res.headers.items())
            body = res.read()
        request_id = headers.get("Lambda-Runtime-Aws-Request-Id")
        trace = headers.get("Lambda-Runtime-Trace-Id")
        if trace:
            os.environ["_X_AMZN_TRACE_ID"] = trace
        else:
            os.environ.pop("_X_AMZN_TRACE_ID", None)
        try:
            event = json.loads(body) if body else None
            result = handler(event, LambdaContext(headers))
            post("/invocation/%s/response" % request_id, result)
        except Exception as exc:
            traceback.print_exc()
            payload = error_payload(exc)
            payload["requestId"] = request_id
            post("/invocation/%s/error" % request_id, payload, {"Lambda-Runtime-Function-Error-Type": "Unhandled"})
        sys.stdout.flush()
        sys.stderr.flush()


if __name__ == "__main__":
    main()
