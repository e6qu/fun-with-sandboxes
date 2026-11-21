package api

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/e6qu/fun-with-sandboxes/internal/engine"
)

const kubeYAML = `apiVersion: v1
kind: ConfigMap
metadata:
  name: settings
data:
  GREETING: hello
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  replicas: 1
  template:
    metadata:
      labels:
        app: web
    spec:
      restartPolicy: OnFailure
      volumes:
      - name: data
        persistentVolumeClaim:
          claimName: data
      - name: scratch
        emptyDir: {}
      initContainers:
      - name: prepare
        image: fws/toolbox:1
        args: [put, /data/ready, "yes"]
        volumeMounts:
        - name: data
          mountPath: /data
      containers:
      - name: server
        image: fws/toolbox:1
        command: [/toolbox]
        args: [serve]
        workingDir: /srv
        env:
        - name: PLAIN
          value: "1"
        - name: GREETING
          valueFrom:
            configMapKeyRef:
              name: settings
              key: GREETING
        ports:
        - containerPort: 8080
          hostPort: 18080
        volumeMounts:
        - name: data
          mountPath: /data
          readOnly: true
        - name: scratch
          mountPath: /scratch
        resources:
          limits:
            memory: 64Mi
            cpu: 500m
        securityContext:
          runAsUser: 1000
          runAsGroup: 50
`

// A Deployment of one replica makes a pod named after it, with its
// template's labels and spec; ConfigMaps and claims are kept by name.
func TestParseKube(t *testing.T) {
	pods, cms, claims, err := parseKube(strings.NewReader(kubeYAML))
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 1 || pods[0].Name != "web-pod" || pods[0].Labels["app"] != "web" || pods[0].Spec.RestartPolicy != "OnFailure" {
		t.Fatalf("pods: %+v", pods)
	}
	if c := pods[0].Spec.Containers[0]; c.Name != "server" || c.Ports[0].HostPort != 18080 || c.Resources.Limits["memory"] != "64Mi" || *c.SecurityContext.RunAsUser != 1000 {
		t.Errorf("container: %+v", c)
	}
	if len(pods[0].Spec.InitContainers) != 1 || len(pods[0].Spec.Volumes) != 2 {
		t.Errorf("init containers and volumes: %+v", pods[0].Spec)
	}
	if cms["settings"]["GREETING"] != "hello" || len(claims) != 1 || claims[0] != "data" {
		t.Errorf("configmaps %v, claims %v", cms, claims)
	}
	for yaml, want := range map[string]engine.Kind{
		"kind: Deployment\nmetadata: {name: x}\nspec: {replicas: 3}\n": engine.KindUnsupported,
		"kind: Service\nmetadata: {name: x}\n":                         engine.KindUnsupported,
		"kind: Pod\nmetadata: {}\nspec: {}\n":                          engine.KindInvalid,
		"kind: Pod\n  bad: [\n":                                        engine.KindInvalid,
	} {
		if _, _, _, err := parseKube(strings.NewReader(yaml)); engine.KindOf(err) != want {
			t.Errorf("%q: %v, want kind %v", yaml, err, want)
		}
	}
}

// The pod's container becomes Podman's specification of it: its command,
// variables from values and ConfigMaps, mounts of each volume kind,
// limits and user.
func TestKubeContainerSpec(t *testing.T) {
	pods, cms, _, err := parseKube(strings.NewReader(kubeYAML))
	if err != nil {
		t.Fatal(err)
	}
	kp := pods[0]
	vols := map[string]kubeVolume{}
	for _, v := range kp.Spec.Volumes {
		vols[v.Name] = v
	}
	spec, err := kubeSpec(kp, pod{ID: "p1", Name: "web-pod"}, kp.Spec.Containers[0], "on-failure", vols, cms)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Name != "web-pod-server" || spec.Pod != "p1" || strings.Join(spec.Entrypoint, " ") != "/toolbox" || strings.Join(spec.Command, " ") != "serve" ||
		spec.WorkDir != "/srv" || spec.User != "1000:50" || spec.RestartPolicy != "on-failure" {
		t.Errorf("spec: %+v", spec)
	}
	if spec.Env["PLAIN"] != "1" || spec.Env["GREETING"] != "hello" {
		t.Errorf("env: %v", spec.Env)
	}
	if len(spec.Volumes) != 1 || spec.Volumes[0].Name != "data" || spec.Volumes[0].Dest != "/data" || strings.Join(spec.Volumes[0].Options, ",") != "ro" {
		t.Errorf("volumes: %+v", spec.Volumes)
	}
	if len(spec.Mounts) != 1 || spec.Mounts[0].Type != "volume" || spec.Mounts[0].Source != "" || spec.Mounts[0].Destination != "/scratch" {
		t.Errorf("mounts: %+v", spec.Mounts)
	}
	rl := spec.ResourceLimits
	if rl == nil || *rl.Memory.Limit != 64<<20 || *rl.CPU.Quota != 50000 || *rl.CPU.Period != 100000 {
		t.Errorf("limits: %+v", rl)
	}
	req, err := dockerCreate(spec)
	if err != nil {
		t.Fatal(err)
	}
	if req.HostConfig.Memory != 64<<20 || req.HostConfig.CPUQuota != 50000 {
		t.Errorf("docker create: memory %d quota %d", req.HostConfig.Memory, req.HostConfig.CPUQuota)
	}
	// A volume of a kind kube play does not read is refused.
	bad := kubePod{Name: "bad", Spec: kubePodSpec{Volumes: []kubeVolume{{Name: "s", Other: map[string]any{"secret": map[string]any{}}}}}}
	if _, _, _, err := (&Server{}).playPod(httptest.NewRequest("POST", "/", nil), bad, nil, nil, false); engine.KindOf(err) != engine.KindUnsupported || !strings.Contains(err.Error(), "secret") {
		t.Errorf("a secret volume: %v", err)
	}
}

func TestParseQuantity(t *testing.T) {
	for in, want := range map[string]float64{"64Mi": 64 << 20, "1Gi": 1 << 30, "500m": 0.5, "2": 2, "1.5": 1.5, "128M": 128e6, "1k": 1000} {
		if got, err := parseQuantity(in); err != nil || got != want {
			t.Errorf("%s: %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "lots", "-1", "Mi"} {
		if _, err := parseQuantity(in); err == nil {
			t.Errorf("%q parsed", in)
		}
	}
}
