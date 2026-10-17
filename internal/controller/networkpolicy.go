package controller

import (
	"fmt"
	"net"
	"net/url"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	hatchetv1alpha1 "github.com/abinnovision/hatchet-controller/api/v1alpha1"
)

// egressPolicySpec allows worker pods egress to the engine and API pods of the instance and to DNS.
func egressPolicySpec(instance *hatchetv1alpha1.HatchetInstance) (networkingv1.NetworkPolicySpec, error) {
	np := instance.Spec.NetworkPolicy
	enginePort, apiPort, err := policyPorts(instance)
	if err != nil {
		return networkingv1.NetworkPolicySpec{}, err
	}
	namespace := &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: instance.Namespace}}
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	port := func(protocol *corev1.Protocol, p int32) networkingv1.NetworkPolicyPort {
		v := intstr.FromInt32(p)
		return networkingv1.NetworkPolicyPort{Protocol: protocol, Port: &v}
	}
	to := func(selector map[string]string, p int32) networkingv1.NetworkPolicyEgressRule {
		return networkingv1.NetworkPolicyEgressRule{
			To:    []networkingv1.NetworkPolicyPeer{{NamespaceSelector: namespace, PodSelector: &metav1.LabelSelector{MatchLabels: selector}}},
			Ports: []networkingv1.NetworkPolicyPort{port(&tcp, p)},
		}
	}
	return networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{hatchetv1alpha1.WorkerLabel: "true"}},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
		Egress: []networkingv1.NetworkPolicyEgressRule{
			to(np.EngineSelector, enginePort),
			to(np.APISelector, apiPort),
			{Ports: []networkingv1.NetworkPolicyPort{port(&udp, 53), port(&tcp, 53)}},
		},
	}, nil
}

// policyPorts returns the engine and API pod ports: the overrides, else the ports of
// worker.hostPort and apiURL, else 443 (80 without TLS or for http).
func policyPorts(instance *hatchetv1alpha1.HatchetInstance) (engine, api int32, err error) {
	if p := instance.Spec.NetworkPolicy.Ports; p != nil {
		engine, api = p.Engine, p.API
	}
	if engine == 0 {
		engine = 443
		if instance.Spec.Worker.TLSStrategy == hatchetv1alpha1.TLSStrategyNone {
			engine = 80
		}
		if _, port, splitErr := net.SplitHostPort(instance.Spec.Worker.HostPort); splitErr == nil {
			if engine, err = parsePort(port); err != nil {
				return 0, 0, fmt.Errorf("worker.hostPort: %w", err)
			}
		}
	}
	if api == 0 {
		u, parseErr := url.Parse(instance.Spec.APIURL)
		if parseErr != nil {
			return 0, 0, fmt.Errorf("apiURL: %w", parseErr)
		}
		api = 443
		if u.Scheme == "http" {
			api = 80
		}
		if u.Port() != "" {
			if api, err = parsePort(u.Port()); err != nil {
				return 0, 0, fmt.Errorf("apiURL: %w", err)
			}
		}
	}
	return engine, api, nil
}

func parsePort(s string) (int32, error) {
	p, err := strconv.ParseInt(s, 10, 32)
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("invalid port %q", s)
	}
	return int32(p), nil
}
