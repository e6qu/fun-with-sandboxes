package engine

import "github.com/e6qu/fun-with-sandboxes/internal/bandwidth"

// containerBandwidth reads a container's rates, in bits a second, from its
// labels, as Kubernetes reads a pod's from its annotations: what it sends
// and what it receives. 0 is no limit.
func containerBandwidth(labels map[string]string) (egress, ingress int64, err error) {
	for _, l := range []struct {
		key string
		to  *int64
	}{{bandwidth.EgressLabel, &egress}, {bandwidth.IngressLabel, &ingress}} {
		v, ok := labels[l.key]
		if !ok {
			continue
		}
		if *l.to, err = bandwidth.ParseRate(v); err != nil {
			return 0, 0, invalid("label %s: %v", l.key, err)
		}
	}
	return egress, ingress, nil
}
