package service

import corev1 "k8s.io/api/core/v1"

// MapK8sProtocolToCMP maps the Kubernetes transport protocol to CMP. Application
// protocol must be selected explicitly through the supported protocol
// annotation; a port number alone does not prove HTTP or HTTPS semantics.
func MapK8sProtocolToCMP(p corev1.Protocol, _ int32) string {
	switch p {
	case corev1.ProtocolUDP:
		return "UDP"
	case corev1.ProtocolTCP:
		return "TCP"
	default:
		return "TCP"
	}
}
