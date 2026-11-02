package fws.lambda;

import com.amazonaws.services.lambda.runtime.ClientContext;
import com.amazonaws.services.lambda.runtime.CognitoIdentity;
import com.amazonaws.services.lambda.runtime.Context;
import com.amazonaws.services.lambda.runtime.LambdaLogger;
import com.amazonaws.services.lambda.runtime.RequestHandler;
import com.amazonaws.services.lambda.runtime.RequestStreamHandler;
import com.fasterxml.jackson.databind.DeserializationFeature;
import com.fasterxml.jackson.databind.JavaType;
import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ArrayNode;
import com.fasterxml.jackson.databind.node.ObjectNode;
import java.io.ByteArrayInputStream;
import java.io.ByteArrayOutputStream;
import java.io.File;
import java.io.InputStream;
import java.io.OutputStream;
import java.lang.reflect.InvocationTargetException;
import java.lang.reflect.Method;
import java.lang.reflect.Modifier;
import java.lang.reflect.Type;
import java.net.URI;
import java.net.URL;
import java.net.URLClassLoader;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;

/**
 * The Java runtime of the fws Lambda images: a client of the Lambda Runtime
 * API that loads the handler and runs each invocation, as AWS's runtime
 * interface client does. The handler is package.Class::method, or
 * package.Class for a RequestHandler or RequestStreamHandler; classes load
 * from LAMBDA_TASK_ROOT and the jars in its lib directory, and events and
 * results go through Jackson.
 */
public final class Bootstrap {
    static final ObjectMapper JSON = new ObjectMapper()
            .configure(DeserializationFeature.FAIL_ON_UNKNOWN_PROPERTIES, false);

    public static void main(String[] args) throws Exception {
        String api = "http://" + System.getenv("AWS_LAMBDA_RUNTIME_API") + "/2018-06-01/runtime";
        HttpClient http = HttpClient.newBuilder().version(HttpClient.Version.HTTP_1_1).build();
        String spec = args.length > 0 ? args[args.length - 1] : System.getenv("_HANDLER");
        Invoker invoker;
        try {
            invoker = Invoker.load(spec, taskLoader());
        } catch (Throwable t) {
            Throwable cause = unwrap(t);
            cause.printStackTrace();
            post(http, api + "/init/error", error(cause), "Runtime.HandlerNotFound");
            System.exit(1);
            return;
        }
        while (true) {
            HttpResponse<byte[]> next = http.send(HttpRequest.newBuilder(URI.create(api + "/invocation/next")).GET().build(),
                    HttpResponse.BodyHandlers.ofByteArray());
            String id = next.headers().firstValue("Lambda-Runtime-Aws-Request-Id").orElse("");
            next.headers().firstValue("Lambda-Runtime-Trace-Id").ifPresentOrElse(
                    v -> System.setProperty("com.amazonaws.xray.traceHeader", v),
                    () -> System.clearProperty("com.amazonaws.xray.traceHeader"));
            try {
                byte[] out = invoker.invoke(next.body(), new LambdaContext(next.headers().map()));
                post(http, api + "/invocation/" + id + "/response", out, null);
            } catch (Throwable t) {
                Throwable cause = unwrap(t);
                cause.printStackTrace();
                post(http, api + "/invocation/" + id + "/error", error(cause), "Unhandled");
            }
            System.out.flush();
            System.err.flush();
        }
    }

    static Throwable unwrap(Throwable t) {
        while (t instanceof InvocationTargetException && t.getCause() != null) {
            t = t.getCause();
        }
        return t;
    }

    static void post(HttpClient http, String url, byte[] body, String errorType) throws Exception {
        HttpRequest.Builder b = HttpRequest.newBuilder(URI.create(url))
                .header("Content-Type", "application/json")
                .POST(HttpRequest.BodyPublishers.ofByteArray(body));
        if (errorType != null) {
            b.header("Lambda-Runtime-Function-Error-Type", errorType);
        }
        http.send(b.build(), HttpResponse.BodyHandlers.discarding());
    }

    static byte[] error(Throwable t) throws Exception {
        ObjectNode n = JSON.createObjectNode();
        n.put("errorMessage", String.valueOf(t.getMessage()));
        n.put("errorType", t.getClass().getName());
        ArrayNode trace = n.putArray("stackTrace");
        for (StackTraceElement e : t.getStackTrace()) {
            if (!e.getClassName().startsWith("fws.lambda.") && !e.getClassName().startsWith("java.") && !e.getClassName().startsWith("jdk.")) {
                trace.add(e.toString());
            }
        }
        return JSON.writeValueAsBytes(n);
    }

    /** The task root's classes and the jars in its lib directory, on the host. */
    static ClassLoader taskLoader() throws Exception {
        String root = System.getenv().getOrDefault("SANDBOX_ROOT", "");
        File task = new File(root + System.getenv().getOrDefault("LAMBDA_TASK_ROOT", "/var/task"));
        List<URL> urls = new ArrayList<>();
        urls.add(task.toURI().toURL());
        File[] jars = new File(task, "lib").listFiles((d, n) -> n.endsWith(".jar"));
        if (jars != null) {
            java.util.Arrays.sort(jars);
            for (File j : jars) {
                urls.add(j.toURI().toURL());
            }
        }
        return new URLClassLoader(urls.toArray(new URL[0]), Bootstrap.class.getClassLoader());
    }

    interface Invoker {
        byte[] invoke(byte[] event, Context ctx) throws Exception;

        static Invoker load(String spec, ClassLoader loader) throws Exception {
            if (spec == null || spec.isEmpty()) {
                throw new IllegalArgumentException("no handler given");
            }
            String className = spec;
            String methodName = null;
            int sep = spec.indexOf("::");
            if (sep >= 0) {
                className = spec.substring(0, sep);
                methodName = spec.substring(sep + 2);
            }
            Class<?> cls = Class.forName(className, true, loader);
            Thread.currentThread().setContextClassLoader(loader);
            if (methodName == null && RequestStreamHandler.class.isAssignableFrom(cls)) {
                RequestStreamHandler h = (RequestStreamHandler) cls.getDeclaredConstructor().newInstance();
                return (event, ctx) -> {
                    ByteArrayOutputStream out = new ByteArrayOutputStream();
                    h.handleRequest(new ByteArrayInputStream(event), out, ctx);
                    return out.toByteArray();
                };
            }
            if (methodName == null && !RequestHandler.class.isAssignableFrom(cls)) {
                throw new IllegalArgumentException("Class " + className + " does not implement RequestHandler or RequestStreamHandler and no method is named");
            }
            String name = methodName == null ? "handleRequest" : methodName;
            Method method = null;
            for (Method m : cls.getMethods()) {
                if (!m.getName().equals(name) || m.isBridge()) {
                    continue;
                }
                // The most parameters wins, as AWS's runtime picks.
                if (method == null || m.getParameterCount() > method.getParameterCount()) {
                    method = m;
                }
            }
            if (method == null) {
                throw new NoSuchMethodException("No public method named " + name + " on class " + className);
            }
            Object target = Modifier.isStatic(method.getModifiers()) ? null : cls.getDeclaredConstructor().newInstance();
            Method m = method;
            Class<?>[] types = m.getParameterTypes();
            Type[] generic = m.getGenericParameterTypes();
            if (types.length >= 2 && InputStream.class.isAssignableFrom(types[0]) && OutputStream.class.isAssignableFrom(types[1])) {
                return (event, ctx) -> {
                    ByteArrayOutputStream out = new ByteArrayOutputStream();
                    if (types.length == 3) {
                        m.invoke(target, new ByteArrayInputStream(event), out, ctx);
                    } else {
                        m.invoke(target, new ByteArrayInputStream(event), out);
                    }
                    return out.toByteArray();
                };
            }
            if (types.length > 2) {
                throw new IllegalArgumentException("Handler " + spec + " takes " + types.length + " parameters");
            }
            JavaType input = types.length == 0 ? null : JSON.getTypeFactory().constructType(generic[0]);
            return (event, ctx) -> {
                Object result;
                if (types.length == 0) {
                    result = m.invoke(target);
                } else {
                    Object in = event.length == 0 ? null : JSON.readValue(event, input);
                    result = types.length == 2 ? m.invoke(target, in, ctx) : m.invoke(target, in);
                }
                return JSON.writeValueAsBytes(result);
            };
        }
    }

    static final class LambdaContext implements Context {
        final Map<String, List<String>> headers;
        final long deadline;

        LambdaContext(Map<String, List<String>> headers) {
            this.headers = headers;
            this.deadline = Long.parseLong(header("Lambda-Runtime-Deadline-Ms", "0"));
        }

        String header(String name, String def) {
            for (Map.Entry<String, List<String>> e : headers.entrySet()) {
                if (e.getKey().equalsIgnoreCase(name) && !e.getValue().isEmpty()) {
                    return e.getValue().get(0);
                }
            }
            return def;
        }

        public String getAwsRequestId() { return header("Lambda-Runtime-Aws-Request-Id", null); }
        public String getLogGroupName() { return System.getenv("AWS_LAMBDA_LOG_GROUP_NAME"); }
        public String getLogStreamName() { return System.getenv("AWS_LAMBDA_LOG_STREAM_NAME"); }
        public String getFunctionName() { return System.getenv("AWS_LAMBDA_FUNCTION_NAME"); }
        public String getFunctionVersion() { return System.getenv("AWS_LAMBDA_FUNCTION_VERSION"); }
        public String getInvokedFunctionArn() { return header("Lambda-Runtime-Invoked-Function-Arn", null); }
        public int getRemainingTimeInMillis() { return (int) Math.max(deadline - System.currentTimeMillis(), 0); }

        public int getMemoryLimitInMB() {
            String v = System.getenv("AWS_LAMBDA_FUNCTION_MEMORY_SIZE");
            return v == null ? 0 : Integer.parseInt(v);
        }

        public CognitoIdentity getIdentity() {
            String v = header("Lambda-Runtime-Cognito-Identity", null);
            if (v == null) {
                return null;
            }
            try {
                JsonNode n = JSON.readTree(v);
                String id = n.path("cognitoIdentityId").asText(null);
                String pool = n.path("cognitoIdentityPoolId").asText(null);
                return new CognitoIdentity() {
                    public String getIdentityId() { return id; }
                    public String getIdentityPoolId() { return pool; }
                };
            } catch (Exception e) {
                return null;
            }
        }

        public ClientContext getClientContext() {
            String v = header("Lambda-Runtime-Client-Context", null);
            if (v == null) {
                return null;
            }
            try {
                JsonNode n = JSON.readTree(v);
                Map<String, String> custom = JSON.convertValue(n.path("custom"), JSON.getTypeFactory().constructMapType(Map.class, String.class, String.class));
                Map<String, String> env = JSON.convertValue(n.path("env"), JSON.getTypeFactory().constructMapType(Map.class, String.class, String.class));
                JsonNode c = n.path("client");
                com.amazonaws.services.lambda.runtime.Client client = new com.amazonaws.services.lambda.runtime.Client() {
                    public String getInstallationId() { return c.path("installation_id").asText(null); }
                    public String getAppTitle() { return c.path("app_title").asText(null); }
                    public String getAppVersionName() { return c.path("app_version_name").asText(null); }
                    public String getAppVersionCode() { return c.path("app_version_code").asText(null); }
                    public String getAppPackageName() { return c.path("app_package_name").asText(null); }
                };
                return new ClientContext() {
                    public com.amazonaws.services.lambda.runtime.Client getClient() { return client; }
                    public Map<String, String> getCustom() { return custom; }
                    public Map<String, String> getEnvironment() { return env; }
                };
            } catch (Exception e) {
                return null;
            }
        }

        public LambdaLogger getLogger() {
            return new LambdaLogger() {
                public void log(String message) {
                    System.out.print(message);
                    System.out.flush();
                }

                public void log(byte[] message) {
                    System.out.write(message, 0, message.length);
                    System.out.flush();
                }
            };
        }
    }
}
