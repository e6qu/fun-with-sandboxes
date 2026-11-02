// The Node.js runtime of the fws Lambda images: a client of the Lambda
// Runtime API that loads the handler and runs each invocation, as AWS's
// runtime interface client does. The handler is file.function, the file
// an .mjs, .js or .cjs module under LAMBDA_TASK_ROOT; it may return a
// promise or take a callback.
import { existsSync } from "node:fs";
import { createRequire } from "node:module";
import path from "node:path";
import { pathToFileURL } from "node:url";

const API = `http://${process.env.AWS_LAMBDA_RUNTIME_API}/2018-06-01/runtime`;

// SANDBOX_ROOT holds the container's root on the host.
function hostPath(p) {
  const root = process.env.SANDBOX_ROOT ?? "";
  return root && !p.startsWith(root + "/") ? root + p : p;
}

async function post(route, payload, headers = {}) {
  const res = await fetch(API + route, {
    method: "POST",
    headers: { "Content-Type": "application/json", ...headers },
    body: payload === undefined ? "null" : JSON.stringify(payload),
  });
  await res.arrayBuffer();
}

function errorPayload(err) {
  if (!(err instanceof Error)) {
    err = new Error(typeof err === "string" ? err : JSON.stringify(err));
  }
  return {
    errorType: err.name ?? "Error",
    errorMessage: err.message ?? String(err),
    trace: (err.stack ?? "").split("\n"),
  };
}

async function loadHandler(spec) {
  const dot = spec.lastIndexOf(".");
  if (dot < 1) {
    throw Object.assign(new Error(`Bad handler ${spec}`), { name: "Runtime.MalformedHandlerName" });
  }
  const base = path.join(hostPath(process.env.LAMBDA_TASK_ROOT ?? "/var/task"), spec.slice(0, dot));
  const fn = spec.slice(dot + 1);
  let mod;
  const file = [".mjs", ".js", ".cjs"].map((ext) => base + ext).find(existsSync);
  if (!file) {
    throw Object.assign(new Error(`Cannot find module '${path.basename(base)}'`), { name: "Runtime.ImportModuleError" });
  }
  if (file.endsWith(".cjs")) {
    mod = createRequire(import.meta.url)(file);
  } else {
    mod = await import(pathToFileURL(file).href);
  }
  const handler = mod[fn] ?? mod.default?.[fn];
  if (typeof handler !== "function") {
    throw Object.assign(new Error(`${spec} is undefined or not exported`), { name: "Runtime.HandlerNotFound" });
  }
  return handler;
}

function context(headers) {
  const deadline = Number(headers.get("lambda-runtime-deadline-ms") ?? 0);
  const client = headers.get("lambda-runtime-client-context");
  const identity = headers.get("lambda-runtime-cognito-identity");
  return {
    callbackWaitsForEmptyEventLoop: true,
    functionName: process.env.AWS_LAMBDA_FUNCTION_NAME,
    functionVersion: process.env.AWS_LAMBDA_FUNCTION_VERSION,
    invokedFunctionArn: headers.get("lambda-runtime-invoked-function-arn"),
    memoryLimitInMB: process.env.AWS_LAMBDA_FUNCTION_MEMORY_SIZE,
    awsRequestId: headers.get("lambda-runtime-aws-request-id"),
    logGroupName: process.env.AWS_LAMBDA_LOG_GROUP_NAME,
    logStreamName: process.env.AWS_LAMBDA_LOG_STREAM_NAME,
    clientContext: client ? JSON.parse(client) : undefined,
    identity: identity ? JSON.parse(identity) : undefined,
    getRemainingTimeInMillis: () => Math.max(deadline - Date.now(), 0),
  };
}

// invoke runs the handler as AWS does: a handler of three parameters
// answers through its callback, any other through its return value.
function invoke(handler, event, ctx) {
  return new Promise((resolve, reject) => {
    let settled = false;
    const callback = (err, result) => {
      if (settled) return;
      settled = true;
      err ? reject(err) : resolve(result);
    };
    ctx.done = callback;
    ctx.succeed = (result) => callback(null, result);
    ctx.fail = (err) => callback(err ?? new Error("handled"));
    try {
      const out = handler(event, ctx, callback);
      if (handler.length < 3 || (out && typeof out.then === "function")) {
        Promise.resolve(out).then((r) => callback(null, r), callback);
      }
    } catch (err) {
      callback(err);
    }
  });
}

const spec = process.argv.length > 2 ? process.argv[process.argv.length - 1] : (process.env._HANDLER ?? "");
let handler;
try {
  handler = await loadHandler(spec);
} catch (err) {
  console.error(err);
  await post("/init/error", errorPayload(err), { "Lambda-Runtime-Function-Error-Type": err.name ?? "Runtime.Unknown" });
  process.exit(1);
}
for (;;) {
  const res = await fetch(API + "/invocation/next");
  const body = await res.text();
  const id = res.headers.get("lambda-runtime-aws-request-id");
  const trace = res.headers.get("lambda-runtime-trace-id");
  if (trace) process.env._X_AMZN_TRACE_ID = trace;
  else delete process.env._X_AMZN_TRACE_ID;
  try {
    const result = await invoke(handler, body ? JSON.parse(body) : undefined, context(res.headers));
    await post(`/invocation/${id}/response`, result);
  } catch (err) {
    console.error(err);
    await post(`/invocation/${id}/error`, errorPayload(err), { "Lambda-Runtime-Function-Error-Type": "Unhandled" });
  }
}
