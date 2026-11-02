//go:build e2e && darwin

package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// invoke sends event to a Lambda image's emulator, as the AWS base images
// take invocations, and returns the function's answer.
func invoke(t *testing.T, name, event string) (int, string) {
	t.Helper()
	url := "http://" + strings.TrimSpace(mustDocker(t, "port", name, "8080")) + "/2015-03-31/functions/function/invocations"
	res, err := http.Post(url, "application/json", strings.NewReader(event))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func lambdaFunction(t *testing.T, name, image string, files map[string]string, handler string) {
	t.Helper()
	dir := t.TempDir()
	for f, body := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	waitedRun(t, name, "-p", "127.0.0.1::8080", "-v", dir+":/var/task:ro", image, handler)
}

func TestLambdaPython(t *testing.T) {
	const name = "fws-e2e-lambda-python"
	lambdaFunction(t, name, "fws/lambda/python:3.14", map[string]string{"app.py": `import sys
def handler(event, context):
    if event.get("fail"):
        raise ValueError("asked to fail")
    return {"greeting": "hello " + event["name"], "python": "%d.%d" % sys.version_info[:2], "function": context.function_name}
`}, "app.handler")
	code, body := invoke(t, name, `{"name":"lambda"}`)
	var got map[string]string
	if code != 200 || json.Unmarshal([]byte(body), &got) != nil || got["greeting"] != "hello lambda" || got["python"] != "3.14" || got["function"] == "" {
		t.Fatalf("invocation: %d %s", code, body)
	}
	if _, body := invoke(t, name, `{"fail":true}`); !strings.Contains(body, `"errorType": "ValueError"`) && !strings.Contains(body, `"errorType":"ValueError"`) {
		t.Fatalf("a failing invocation answered %s", body)
	}
}

func TestLambdaNode(t *testing.T) {
	const name = "fws-e2e-lambda-node"
	lambdaFunction(t, name, "fws/lambda/nodejs:24", map[string]string{"index.mjs": `export const handler = async (event, context) => ({
  greeting: "hello " + event.name,
  node: process.versions.node.split(".")[0],
  requestId: context.awsRequestId,
});
`}, "index.handler")
	code, body := invoke(t, name, `{"name":"lambda"}`)
	var got map[string]string
	if code != 200 || json.Unmarshal([]byte(body), &got) != nil || got["greeting"] != "hello lambda" || got["node"] != "24" || got["requestId"] == "" {
		t.Fatalf("invocation: %d %s", code, body)
	}
}

// The Java image carries a JDK: a build compiles the handler against the
// runtime's libraries, as a function's own build would.
func TestLambdaJava(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "App.java"), []byte(`package example;

import com.amazonaws.services.lambda.runtime.Context;
import com.amazonaws.services.lambda.runtime.RequestHandler;

public class App implements RequestHandler<App.Request, App.Response> {
    public static class Request { public String name; }
    public static class Response {
        public String greeting;
        public String java;
        public String requestId;
    }

    public Response handleRequest(Request in, Context ctx) {
        Response r = new Response();
        r.greeting = "hello " + in.name;
        r.java = System.getProperty("java.specification.version");
        r.requestId = ctx.getAwsRequestId();
        return r;
    }
}
`), 0o644)
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(`FROM fws/lambda/java:17
COPY App.java ./
RUN ["javac", "-cp", "../runtime/lib/*", "-d", ".", "App.java"]
CMD ["example.App::handleRequest"]
`), 0o644)
	if out, err := exec.Command("docker", "buildx", "build", "--load", "-t", "e2e/lambda-java:1", dir).CombinedOutput(); err != nil {
		t.Fatalf("build the function: %v\n%s", err, out)
	}
	defer docker(t, "rmi", "e2e/lambda-java:1")
	const name = "fws-e2e-lambda-java"
	waitedRun(t, name, "-p", "127.0.0.1::8080", "e2e/lambda-java:1")
	code, body := invoke(t, name, `{"name":"lambda"}`)
	var got map[string]string
	if code != 200 || json.Unmarshal([]byte(body), &got) != nil || got["greeting"] != "hello lambda" || got["java"] != "17" || got["requestId"] == "" {
		t.Fatalf("invocation: %d %s", code, body)
	}
}
